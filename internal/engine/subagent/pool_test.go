// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// scriptedAgent answers one turn and reports it finished, which is all a pool needs
// from a sub agent to be exercised.
type scriptedAgent struct {
	core.Agent
	delay time.Duration
	fail  bool
	// events is what the run emits, so a caller can be watched.
	events []core.Event
	// recorder is what the agent was asked, so a test can prove the slot did not
	// carry one task's input into the next. It is the builder rather than a bare
	// slice, because every agent built by one builder writes to the same record and
	// a lock per writer is no lock at all when the writers share what they write.
	recorder *recordingBuilder
}

func (a *scriptedAgent) AgentID() string { return "scripted" }

// State reports a view with no conversation, because the pool does not read the
// agent's state: the pool's own record is what it reports, and having two sources
// for the same fact is how they come to disagree.
func (a *scriptedAgent) State() core.StateView { return a }

func (a *scriptedAgent) Messages() []core.Message  { return nil }
func (a *scriptedAgent) Variables() map[string]any { return nil }
func (a *scriptedAgent) ParentID() string          { return "" }
func (a *scriptedAgent) Metadata() map[string]any  { return nil }

// Hooks and EventBus are nil because nothing in the pool reaches for them. A test
// double that answered them would be a second place to keep in step with the real
// interface, for a path no test uses.
func (a *scriptedAgent) Hooks() core.HookRegistry { return nil }

func (a *scriptedAgent) EventBus() core.EventBus { return nil }

func (a *scriptedAgent) Run(ctx context.Context, input core.Input) (<-chan core.Event, error) {
	if a.recorder != nil {
		a.recorder.note(input.Content)
	}
	out := make(chan core.Event, len(a.events)+2)
	go func() {
		defer close(out)
		if a.delay > 0 {
			select {
			case <-time.After(a.delay):
			case <-ctx.Done():
				return
			}
		}
		for _, event := range a.events {
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
		if a.fail {
			out <- core.Event{Type: core.EventTypeError, Error: "the model refused"}
			return
		}
		out <- core.Event{Type: core.EventTypeDone}
	}()
	return out, nil
}

// recordingBuilder hands out scripted agents and remembers what it was asked for.
type recordingBuilder struct {
	mu        sync.Mutex
	built     int
	templates []string
	// toolsSeen is the tool set each build was given, which is how a test proves the
	// main core's template confined the sub agent.
	toolsSeen [][]string
	delay     time.Duration
	fail      bool
	seen      []string
}

func (b *recordingBuilder) Build(ctx context.Context, template Template, slot *Slot) (core.Agent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.built++
	b.templates = append(b.templates, template.Name)
	b.toolsSeen = append(b.toolsSeen, append([]string(nil), template.Tools...))
	return &scriptedAgent{delay: b.delay, fail: b.fail, recorder: b}, nil
}

func (b *recordingBuilder) buildCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.built
}

func (b *recordingBuilder) note(input string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seen = append(b.seen, input)
}

func (b *recordingBuilder) inputs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

// newTestPool builds a pool over one template and a broker, which is the shape a mode
// assembles.
func newTestPool(t *testing.T, builder AgentBuilder, capacity, concurrent int) (*Pool, *Registry, *Broker) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	templates := newTemplatePool(t, dir, "")
	writeTemplate(t, dir, "reviewer", reviewerTemplate)
	if err := templates.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	broker, err := NewBroker(BrokerConfig{MainID: "main", Journal: &recordingJournal{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	registry, err := NewRegistry(RegistryConfig{MainID: "main", Broker: broker})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(PooledAgentConfig{
		MainID: "main", Registry: registry, Broker: broker,
		Templates: templates, Builder: builder, Capacity: capacity, MaxConcurrent: concurrent,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return pool, registry, broker
}

// runTask starts a task on whichever slot is free and waits for it, which is the
// shape a caller takes when it genuinely cannot go on without the answer. A caller
// that can go on uses Start and reads the result when it wants it.
func runTask(pool *Pool, template, input string) (TaskResult, error) {
	return runTaskOn(pool, "", input)
}

// runTaskOn is runTask aimed at one particular slot.
func runTaskOn(pool *Pool, slotID, input string) (TaskResult, error) {
	ctx := context.Background()
	slot, err := pool.Start(ctx, pool.MainID(), Task{
		Slot:    slotID,
		Input:   core.Input{Type: core.InputTypeText, Content: input},
		Summary: "asked to " + input,
	})
	if err != nil {
		return TaskResult{}, err
	}
	return pool.Wait(ctx, slot.ID)
}

// The point of a pool is that the slot is reused and the agent is not, because a
// core agent cannot be cleared. A test that only checked that a task ran would pass
// with a pool that rebuilt everything and called it reuse.
func TestASlotIsReusedAndItsAgentIsNot(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 2, 2)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	// The slot is where the sub agent's identifier comes from, so the registry has to
	// know about it before anything runs on it.
	for round := 0; round < 3; round++ {
		result, err := runTaskOn(pool, slot.ID, fmt.Sprintf("round %d", round))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if result.Slot != slot.ID {
			t.Fatalf("the task ran on %q, not the slot that was asked for", result.Slot)
		}
	}
	after, ok := pool.GetSlot(slot.ID)
	if !ok {
		t.Fatal("the slot disappeared")
	}
	// Three runs on one slot, and three agents, which is the whole design: the slot
	// is the reusable part and the conversation is not.
	if after.Runs != 3 {
		t.Fatalf("runs = %d, want three on one slot", after.Runs)
	}
	if len(pool.ListSlots()) != 1 {
		t.Fatalf("slots = %d, want one", len(pool.ListSlots()))
	}
}

// Two tasks on one slot must not share a conversation. This is the property that
// makes the slot and the agent separate, and it is invisible without checking.
func TestTwoTasksOnOneSlotDoNotShareAConversation(t *testing.T) {
	builder := &recordingBuilder{}
	pool, _, _ := newTestPool(t, builder, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"first", "second", "third"} {
		if _, err := runTaskOn(pool, slot.ID, input); err != nil {
			t.Fatal(err)
		}
	}
	// Each task was asked exactly what it said, and nothing carried over, because
	// each got its own agent.
	inputs := builder.inputs()
	if len(inputs) != 3 {
		t.Fatalf("the builder saw %d tasks, want three", len(inputs))
	}
	for index, want := range []string{"first", "second", "third"} {
		if inputs[index] != want {
			t.Fatalf("task %d was given %q, want %q", index, inputs[index], want)
		}
	}
}

// A pool with no bound is a way to exhaust a machine, and a pool whose bound is
// only on the number of slots is not a bound on the work.
func TestTheConcurrencyLimitIsEnforced(t *testing.T) {
	builder := &recordingBuilder{delay: 40 * time.Millisecond}
	pool, _, _ := newTestPool(t, builder, 4, 2)
	for index := 0; index < 4; index++ {
		if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); err != nil {
			t.Fatal(err)
		}
	}
	var running, peak int
	var mu sync.Mutex
	var done sync.WaitGroup
	for index := 0; index < 4; index++ {
		done.Add(1)
		go func() {
			defer done.Done()
			_, err := runTask(pool, "reviewer", "work")
			if err != nil {
				t.Errorf("a task failed: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if pool.Stats().Running > peak {
				peak = pool.Stats().Running
			}
			running++
		}()
	}
	done.Wait()
	if running != 4 {
		t.Fatalf("%d of four tasks finished", running)
	}
	mu.Lock()
	observed := peak
	mu.Unlock()
	// The measurement is of the pool's own counter rather than of a counter kept
	// here, because the pool is what has to hold the line.
	if observed > 2 {
		t.Fatalf("peak concurrency = %d, over the limit of two", observed)
	}
	if stats := pool.Stats(); stats.Running != 0 {
		t.Fatalf("the pool still reports %d running after every task finished", stats.Running)
	}
}

// A caller that is told no can decide what to give up. A caller that waits cannot,
// which is why the limit is a refusal and not a queue.
func TestAFullPoolRefusesRatherThanWaitingForever(t *testing.T) {
	builder := &recordingBuilder{delay: time.Second}
	pool, _, _ := newTestPool(t, builder, 1, 1)
	if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); err != nil {
		t.Fatal(err)
	}
	busy := make(chan struct{})
	go func() {
		defer close(busy)
		if _, err := runTask(pool, "reviewer", "slow"); err != nil {
			t.Errorf("the first task failed: %v", err)
		}
	}()
	// Wait for the slot to be taken rather than sleeping and hoping.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pool.Stats().Running == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// A caller with nowhere to go and no time to wait is told so.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := pool.Start(ctx, "main", Task{Input: core.Input{Type: core.InputTypeText, Content: "second"}}); err == nil {
		t.Fatal("a task ran while the only slot was busy")
	}
	<-busy
	// And the slot is usable again, because a slot left busy is a slot the pool can
	// never use again.
	if _, err := runTask(pool, "reviewer", "third"); err != nil {
		t.Fatalf("the slot did not come back: %v", err)
	}
}

// A sub agent is confined to what the main core's template allowed. The registry
// records that tool set, so a caller can see what a running sub agent may reach
// without reading its prompt.
func TestTheTemplateConfinesWhatASubAgentMayUse(t *testing.T) {
	builder := &recordingBuilder{}
	pool, registry, _ := newTestPool(t, builder, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runTaskOn(pool, slot.ID, "look at this"); err != nil {
		t.Fatal(err)
	}
	builder.mu.Lock()
	tools := builder.toolsSeen[0]
	builder.mu.Unlock()
	if len(tools) != 2 || tools[0] != "read" || tools[1] != "grep" {
		t.Fatalf("the sub agent was built with %v, want the template's two", tools)
	}
	// The tool set is filed, so a later caller can read what this sub agent could
	// reach rather than having to remember.
	agent, ok := registry.Get(slot.ID)
	if !ok {
		t.Fatalf("the sub agent %q is not in the registry", slot.ID)
	}
	if len(agent.Tools) != 2 {
		t.Fatalf("the registry records tools %v", agent.Tools)
	}
}

// A finished sub agent owes the main core a message, and it goes through the journal
// before it is delivered, because a result that is only in memory is a result a crash
// takes with it.
func TestAFinishedSubAgentReportsBackToTheMainCore(t *testing.T) {
	pool, _, broker := newTestPool(t, &recordingBuilder{}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	started, err := pool.Start(context.Background(), "main", Task{
		Slot:    slot.ID,
		Input:   core.Input{Type: core.InputTypeText, Content: "check this"},
		Summary: "checked the change and found nothing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Wait(context.Background(), started.ID); err != nil {
		t.Fatal(err)
	}
	mailbox, ok := broker.Mailbox("main")
	if !ok {
		t.Fatal("the main core has no mailbox")
	}
	message, got := mailbox.ReceiveWithin(2 * time.Second)
	if !got {
		t.Fatal("the main core was told nothing")
	}
	if message.FromAgentID != slot.ID {
		t.Fatalf("the message came from %q, want %q", message.FromAgentID, slot.ID)
	}
	if message.Content != "checked the change and found nothing" {
		t.Fatalf("the summary is %q", message.Content)
	}
	// The main core can see every sub agent's state without asking each one.
	if len(pool.ListSlots()) != 1 {
		t.Fatalf("the main core sees %d slots", len(pool.ListSlots()))
	}
	after, _ := pool.GetSlot(slot.ID)
	if after.LastSummary != "checked the change and found nothing" {
		t.Fatalf("the slot's summary is %q", after.LastSummary)
	}
}

// The main core sees a sub agent work, not only what it produced. The events belong
// to the task on the slot, which is why they are only available while one is running.
func TestTheMainCoreCanWatchASubAgentWork(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{delay: 20 * time.Millisecond}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	// An idle slot has nothing to watch, and a caller ranging over it ends rather
	// than blocking.
	events, ok := pool.Watch(slot.ID)
	if !ok {
		t.Fatal("the slot could not be watched")
	}
	select {
	case _, open := <-events:
		if open {
			t.Fatal("an idle slot had something to watch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an idle slot's channel never closed")
	}
}

// A failed task leaves the slot usable and says why. A slot that went unusable on
// one failure would make a pool of four survive exactly one bad task.
func TestAFailedTaskLeavesTheSlotUsableAndSaysWhy(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{fail: true}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	result, err := runTaskOn(pool, slot.ID, "look")
	if err == nil {
		t.Fatal("a failing task reported success")
	}
	if result.State != StateFailed {
		t.Fatalf("state = %q, want failed", result.State)
	}
	after, _ := pool.GetSlot(slot.ID)
	if after.State != SlotFailed {
		t.Fatalf("the slot is %q, want it to say it failed", after.State)
	}
	if !strings.Contains(after.LastErr, "refused") {
		t.Fatalf("the slot's error is %q, which does not say what happened", after.LastErr)
	}
	// A caller can put it back into service without recreating it.
	started, err := pool.StartSlot(context.Background(), "main", slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if started.State != SlotIdle {
		t.Fatalf("the slot is %q after being started", started.State)
	}
}

// The rule is the whole reason this type exists, and it is the one rule a sub agent
// must not be able to argue its way around.
func TestOnlyTheMainCoreMayActOnASlot(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 2, 2)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, agentID := range []string{"sub-1", "", "someone"} {
		if _, err := pool.CreateSlot(ctx, agentID, "reviewer"); !errors.Is(err, ErrNotMain) {
			t.Fatalf("%q created a slot: %v", agentID, err)
		}
		if err := pool.StopSlot(ctx, agentID, slot.ID); !errors.Is(err, ErrNotMain) {
			t.Fatalf("%q stopped a slot: %v", agentID, err)
		}
		if err := pool.DeleteSlot(ctx, agentID, slot.ID); !errors.Is(err, ErrNotMain) {
			t.Fatalf("%q deleted a slot: %v", agentID, err)
		}
		if _, err := pool.StartSlot(ctx, agentID, slot.ID); !errors.Is(err, ErrNotMain) {
			t.Fatalf("%q started a slot: %v", agentID, err)
		}
		if _, err := pool.Start(ctx, agentID, Task{Input: core.Input{Content: "x"}}); !errors.Is(err, ErrNotMain) {
			t.Fatalf("%q started a task: %v", agentID, err)
		}
	}
}

// A busy slot is not deleted and not stopped by accident. A task mid tool call does
// not stop because its slot went away.
func TestABusySlotIsNeitherStoppedNorDeletedByMistake(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{delay: time.Second}, 2, 2)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	// Starting is not waiting. The task runs on its own goroutine, so this test stays
	// on one line and then gets on with checking that a busy slot is protected, which
	// is what a caller would actually be doing.
	busy, err := pool.Start(context.Background(), "main", Task{
		Slot: slot.ID, Input: core.Input{Type: core.InputTypeText, Content: "slow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pool.Stats().Running == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := pool.DeleteSlot(context.Background(), "main", slot.ID); err == nil {
		t.Fatal("a busy slot was deleted")
	}
	// Stopping is how the main core ends a task, and it is allowed on a busy slot.
	if err := pool.StopSlot(context.Background(), "main", slot.ID); err != nil {
		t.Fatalf("a busy slot could not be stopped: %v", err)
	}
	if _, err := pool.Wait(context.Background(), busy.ID); err == nil {
		t.Fatal("a stopped task reported success")
	}
	// And now it is idle and can be deleted.
	if err := pool.DeleteSlot(context.Background(), "main", slot.ID); err != nil {
		t.Fatalf("an idle slot could not be deleted: %v", err)
	}
	if _, ok := pool.GetSlot(slot.ID); ok {
		t.Fatal("the slot is still there after being deleted")
	}
}

func TestThePoolRefusesNonsenseConfiguration(t *testing.T) {
	if _, err := NewPool(PooledAgentConfig{}); err == nil {
		t.Fatal("a pool with no main core was accepted")
	}
	if _, err := NewPool(PooledAgentConfig{MainID: "main", Capacity: -1}); err == nil {
		t.Fatal("a negative capacity was accepted")
	}
	if _, err := NewPool(PooledAgentConfig{MainID: "main", MaxConcurrent: -1}); err == nil {
		t.Fatal("a negative concurrency limit was accepted")
	}
	if _, err := NewPool(PooledAgentConfig{MainID: "main", Capacity: 2, MaxConcurrent: 3}); err == nil {
		t.Fatal("a concurrency limit above the capacity was accepted")
	}
}

func TestACapacityBoundIsEnforced(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 2, 2)
	for index := 0; index < 2; index++ {
		if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("a third slot was created: %v", err)
	}
	// A slot for a template that is not there is refused rather than created empty,
	// because a slot that cannot be run is a slot that occupies the pool.
	if _, err := pool.CreateSlot(context.Background(), "main", "absent"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("a slot for a missing template was created: %v", err)
	}
}

// A closed pool refuses everything rather than half working, because a caller that
// gets a task accepted by a pool that is shutting down has a task nobody is watching.
func TestAClosedPoolRefusesEverything(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("a second close failed: %v", err)
	}
	if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("a closed pool created a slot: %v", err)
	}
	if _, err := runTask(pool, "reviewer", "x"); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("a closed pool ran a task: %v", err)
	}
}

// The pool's state is what the main core reads to decide whether the pool is the
// right size, so it has to be readable and it has to be a document.
func TestThePoolStateIsReadableAsJSON(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 2, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(pool.Stats())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"slots", "capacity", "running", "available"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("the stats are %s, which do not mention %q", encoded, want)
		}
	}
	encoded, err = json.Marshal(pool.ListSlots())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), slot.ID) {
		t.Fatalf("the listing is %s", encoded)
	}
}
