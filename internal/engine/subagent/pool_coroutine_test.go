// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// Starting a sub agent must not cost the caller the time the sub agent spends. This
// is the reason a sub agent is a goroutine, so a test that only checked that a task
// eventually produced a result would pass against the blocking shape this replaced.
func TestStartingASubAgentDoesNotWaitForIt(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{delay: 400 * time.Millisecond}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	busy, err := pool.Start(context.Background(), "main", Task{
		Slot:  slot.ID,
		Input: core.Input{Type: core.InputTypeText, Content: "slow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("starting took %s, so the caller waited on the sub agent", elapsed)
	}
	// The slot is busy from the moment Start returns, not from whenever the sub agent
	// got round to it, or a caller could hand the same slot a second task.
	if busy.State != SlotBusy {
		t.Fatalf("the slot is %q, not busy", busy.State)
	}
	if _, err := pool.Wait(context.Background(), busy.ID); err != nil {
		t.Fatal(err)
	}
}

// The result is readable twice: once by waiting, and once afterwards by asking. A main
// core that was busy when the answer arrived should not have to have been waiting.
func TestTheResultOutlivesTheTaskThatProducedIt(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.Result(slot.ID); ok {
		t.Fatal("a slot that never ran had a result")
	}
	if _, err := runTaskOn(pool, slot.ID, "look"); err != nil {
		t.Fatal(err)
	}
	result, ok := pool.Result(slot.ID)
	if !ok {
		t.Fatal("a slot that ran had no result")
	}
	if result.Summary != "asked to look" {
		t.Fatalf("the result says %q", result.Summary)
	}
	// Waiting again gives the same answer rather than blocking forever or inventing a
	// second one, because the task is over and there is nothing left to wait for.
	again, err := pool.Wait(context.Background(), slot.ID)
	if err != nil {
		t.Fatalf("waiting on a finished task: %v", err)
	}
	if again.Summary != result.Summary {
		t.Fatalf("waiting said %q and asking said %q", again.Summary, result.Summary)
	}
}

// The main core asks a slot things by name, and a name that is not there has to be
// reported rather than answered for a slot that happens to exist.
func TestAskingAboutASlotThatIsNotThere(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	ctx := context.Background()
	if _, ok := pool.GetSlot("no-such-slot"); ok {
		t.Fatal("an absent slot was found")
	}
	if _, ok := pool.Result("no-such-slot"); ok {
		t.Fatal("an absent slot had a result")
	}
	if _, err := pool.Wait(ctx, "no-such-slot"); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("waiting on an absent slot: %v", err)
	}
	// Watching an absent slot ends rather than blocking, so a caller's loop over the
	// events finishes instead of hanging on a channel nobody will ever close.
	events, ok := pool.Watch("no-such-slot")
	if ok {
		t.Fatal("an absent slot was watched")
	}
	select {
	case _, open := <-events:
		if open {
			t.Fatal("an absent slot's channel had an event")
		}
	case <-time.After(time.Second):
		t.Fatal("an absent slot's channel never closed")
	}
	for _, err := range []error{
		pool.StopSlot(ctx, "main", "no-such-slot"),
		pool.DeleteSlot(ctx, "main", "no-such-slot"),
	} {
		if !errors.Is(err, ErrNoSlot) {
			t.Fatalf("acting on an absent slot: %v", err)
		}
	}
	if _, err := pool.StartSlot(ctx, "main", "no-such-slot"); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("starting an absent slot: %v", err)
	}
}

// A pool that cannot build an agent must say so when work is offered, not accept it
// and leave the main core waiting for an answer that cannot come.
func TestAPoolThatCannotRunSaysSoWhenWorkIsOffered(t *testing.T) {
	ctx := context.Background()
	// No builder, so nothing can run however many slots exist.
	unbuildable, err := NewPool(PooledAgentConfig{
		MainID: "main", Templates: templatesFor(t), Capacity: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unbuildable.Close() })
	if _, err := unbuildable.CreateSlot(ctx, "main", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := unbuildable.Start(ctx, "main", Task{Input: core.Input{Content: "x"}}); err == nil {
		t.Fatal("a pool with no builder accepted work")
	}

	// A builder, but no template pool, so the instructions cannot be read.
	templateless, err := NewPool(PooledAgentConfig{
		MainID: "main", Builder: &recordingBuilder{}, Capacity: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = templateless.Close() })
	if _, err := templateless.CreateSlot(ctx, "main", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := templateless.Start(ctx, "main", Task{Input: core.Input{Content: "x"}}); err == nil {
		t.Fatal("a pool with no templates accepted work")
	}
}

// A template that has gone missing, or been rewritten into something invalid, is read
// when the task starts. A slot whose template cannot be read must come straight back
// into service rather than sit there holding a claim it will never use.
func TestASlotWhoseTemplateCannotBeReadComesBack(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	pool.config.Templates = nil
	// The slot was claimed before the template was read, so this is the path where a
	// released claim matters.
	for range 3 {
		if _, err := pool.Start(context.Background(), "main", Task{
			Slot:  slot.ID,
			Input: core.Input{Type: core.InputTypeText, Content: "look"},
		}); err == nil {
			t.Fatal("a task ran with no template pool")
		}
	}
	if got := pool.Stats(); got.Running != 0 {
		t.Fatalf("%d claims were never released", got.Running)
	}
	if got, _ := pool.GetSlot(slot.ID); got.State == SlotBusy {
		t.Fatal("the slot stayed busy after three failures to start")
	}
}

// A builder that fails is a task that failed, not a pool that broke. The slot comes
// back into service and the reason is written down.
func TestAnAgentThatCannotBeBuiltIsAFailedTask(t *testing.T) {
	pool, registry, _ := newTestPool(t, &failingBuilder{}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	result, err := runTaskOn(pool, slot.ID, "look")
	if err == nil {
		t.Fatal("a task that could not build an agent reported success")
	}
	if result.State != StateFailed {
		t.Fatalf("the result is %q", result.State)
	}
	after, ok := pool.GetSlot(slot.ID)
	if !ok {
		t.Fatal("the slot went away")
	}
	if after.LastErr == "" {
		t.Fatal("the slot did not say why it failed")
	}
	if after.State == SlotBusy {
		t.Fatal("the slot stayed busy")
	}
	// The registry agrees with the pool, because a main core that reads one and not
	// the other would be told two different things about the same sub agent.
	agent, ok := registry.Get(slot.ID)
	if !ok {
		t.Fatal("the registry lost the slot")
	}
	if agent.State != StateFailed {
		t.Fatalf("the registry says %q", agent.State)
	}
}

// The sub agent's own view of the world is what a caller watches, so the main core
// sees the work rather than only its outcome.
func TestTheMainCoreSeesTheEventsWhileTheTaskRuns(t *testing.T) {
	pool, _, _ := newTestPool(t, &scriptedBuilder{events: []core.Event{
		{Type: "partial", Data: "working"},
		{Type: "partial", Data: "still working"},
	}}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	busy, err := pool.Start(context.Background(), "main", Task{
		Slot:  slot.ID,
		Input: core.Input{Type: core.InputTypeText, Content: "look"},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, ok := pool.Watch(busy.ID)
	if !ok {
		t.Fatal("the running slot could not be watched")
	}
	seen := 0
	for range events {
		seen++
	}
	if seen == 0 {
		t.Fatal("the main core watched a task and saw nothing")
	}
	if _, err := pool.Wait(context.Background(), busy.ID); err != nil {
		t.Fatal(err)
	}
}

// The accessors are how a caller reaches the parts, and returning the wrong one
// silently would be worse than not offering them.
func TestThePartsAreTheOnesThePoolWasGiven(t *testing.T) {
	registry, err := NewRegistry(RegistryConfig{MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewBroker(BrokerConfig{MainID: "main", Journal: &recordingJournal{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	pool, err := NewPool(PooledAgentConfig{
		MainID: "main", Registry: registry, Broker: broker,
		Templates: templatesFor(t), Builder: &recordingBuilder{}, Capacity: 3, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if pool.Registry() != registry {
		t.Fatal("the pool handed back a different registry")
	}
	if pool.Broker() != broker {
		t.Fatal("the pool handed back a different broker")
	}
	// The capacity asked for is the capacity kept, not the zero a caller gets when its
	// value is dropped on the way in.
	if pool.Capacity() != 3 {
		t.Fatalf("the capacity is %d", pool.Capacity())
	}
	if got := pool.Stats(); got.Capacity != 3 || got.Available != 3 {
		t.Fatalf("the stats say %d of %d free", got.Available, got.Capacity)
	}
}

// A stopped slot stays stopped. It is not silently returned to service, because a
// caller that stopped a sub agent did so because it should not be used.
func TestAStoppedSlotIsNotQuietlyPutBackToWork(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.StopSlot(context.Background(), "main", slot.ID); err != nil {
		t.Fatal(err)
	}
	// A stopped slot will not take a task named for it, and the pool will not quietly
	// hand it one for any other name either.
	if _, err := pool.Start(context.Background(), "main", Task{
		Slot:  slot.ID,
		Input: core.Input{Type: core.InputTypeText, Content: "look"},
	}); err == nil {
		t.Fatal("a stopped slot took a task")
	}
	if got := pool.Stats(); got.Stopped != 1 {
		t.Fatalf("the stats count %d stopped slots", got.Stopped)
	}
	// Stopping a slot that is already stopped is not an error worth failing over.
	if err := pool.StopSlot(context.Background(), "main", slot.ID); err != nil {
		t.Fatalf("stopping a stopped slot: %v", err)
	}
}

// A closed pool takes no more work. Saying so plainly is better than accepting a task
// that will never be run.
func TestAClosedPoolTakesNoMoreWork(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("closing twice: %v", err)
	}
	ctx := context.Background()
	if _, err := pool.CreateSlot(ctx, "main", "reviewer"); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("creating on a closed pool: %v", err)
	}
	if _, err := pool.Start(ctx, "main", Task{Input: core.Input{Content: "x"}}); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("starting on a closed pool: %v", err)
	}
}

// templatesFor builds a pool holding the one template these tests need.
func templatesFor(t *testing.T) *TemplatePool {
	t.Helper()
	dir := t.TempDir()
	templates := newTemplatePool(t, dir, "")
	writeTemplate(t, dir, "reviewer", reviewerTemplate)
	if err := templates.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return templates
}

// failingBuilder cannot make an agent, which is a different failure from an agent that
// runs and then fails.
type failingBuilder struct{}

func (failingBuilder) Build(ctx context.Context, template Template, slot *Slot) (core.Agent, error) {
	return nil, errors.New("no provider is configured")
}

// scriptedBuilder makes an agent that emits a fixed set of events.
type scriptedBuilder struct {
	events []core.Event
}

func (b scriptedBuilder) Build(ctx context.Context, template Template, slot *Slot) (core.Agent, error) {
	return &scriptedAgent{events: b.events}, nil
}
