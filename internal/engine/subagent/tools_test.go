// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// The orchestration tools are the main core's only way to direct sub agents, so the
// whole shape of delegating has to work through them: place a sub agent, give it
// work, watch it run, collect the answer, take it away again.
func TestTheMainCoreCanDelegateAWholeTaskThroughTheTools(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 2, 2)
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]core.Tool{}
	for _, tool := range orchestrator.Tools() {
		tools[tool.Name()] = tool
	}

	spawned := run(t, tools["spawn_subagent"], `{"template":"reviewer"}`)
	id := field(t, spawned, "id")
	if id == "" {
		t.Fatal("spawning returned no id")
	}

	// The sub agent exists but has done nothing, because placing and starting are
	// separate acts and a tool that did both would take the choice away.
	if view := pool.View(); view.SubAgents[0].Runs != 0 {
		t.Fatalf("a sub agent that was only spawned has run %d tasks", view.SubAgents[0].Runs)
	}

	started := run(t, tools["start_subagent"],
		`{"id":"`+id+`","task":"read the file and say what it does","summary":"read the file"}`)
	if got := field(t, started, "state"); got != string(SlotBusy) {
		t.Fatalf("starting left the sub agent %q", got)
	}

	collected := run(t, tools["read_subagent_result"], `{"id":"`+id+`"}`)
	if got := field(t, collected, "summary"); got != "read the file" {
		t.Fatalf("the result says %q", got)
	}

	listed := run(t, tools["list_subagents"], `{}`)
	if !strings.Contains(string(listed.Output), "reviewer") {
		t.Fatal("the listing did not mention the sub agent's template")
	}

	stopped := run(t, tools["stop_subagent"], `{"id":"`+id+`"}`)
	if got := field(t, stopped, "state"); got != string(SlotStopped) {
		t.Fatalf("stopping left the sub agent %q", got)
	}
	// A stopped sub agent is idle, so it can be cleaned up. The rule that protects
	// work in flight is about a busy slot, and a stopped one has nothing in flight.
	if deleted := run(t, tools["delete_subagent"], `{"id":"`+id+`"}`); deleted.IsError {
		t.Fatalf("a stopped sub agent could not be deleted: %s", deleted.Error)
	}
	if _, ok := pool.GetSlot(id); ok {
		t.Fatal("the sub agent is still there after being deleted")
	}
}

// A tool that is refused must say so in its answer rather than as a fault, because a
// model that asked for something it may not have needs to read the reason and act on
// it, not retry or give up.
func TestARefusalIsAnAnswerAndNotAFault(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]core.Tool{}
	for _, tool := range orchestrator.Tools() {
		tools[tool.Name()] = tool
	}

	cases := []struct {
		tool string
		call string
		says string
	}{
		{"spawn_subagent", `{"template":"no-such-template"}`, "template not found"},
		{"spawn_subagent", `{"template":""}`, "needs a template"},
		{"start_subagent", `{"task":""}`, "needs a task"},
		{"stop_subagent", `{"id":"no-such-slot"}`, "no such slot"},
		{"delete_subagent", `{"id":"no-such-slot"}`, "no such slot"},
		{"read_subagent_result", `{"id":"no-such-slot"}`, "no such slot"},
		{"spawn_subagent", `{"template":`, "do not fit"},
	}
	for _, testCase := range cases {
		result := run(t, tools[testCase.tool], testCase.call)
		if !result.IsError {
			t.Fatalf("%s(%s) was not a refusal", testCase.tool, testCase.call)
		}
		if result.Error == "" {
			t.Fatalf("%s(%s) refused without saying why", testCase.tool, testCase.call)
		}
		if !strings.Contains(result.Error, testCase.says) {
			t.Fatalf("%s(%s) said %q, not %q",
				testCase.tool, testCase.call, result.Error, testCase.says)
		}
	}
}

// A sub agent's mailbox is its own. A sub agent that could read another's would be
// able to swallow a result meant for the main core, so the rule is refused and not
// quietly allowed.
func TestASubAgentCannotTakeAnotherSubAgentsMessages(t *testing.T) {
	pool, _, broker := newTestPool(t, &recordingBuilder{}, 2, 2)
	first, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	// A message for the second sub agent, which the first would like to swallow.
	if _, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: second.ID, Type: "task", Content: "mine",
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Take(ctx, first.ID, second.ID, 0); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("one sub agent took another's messages: %v", err)
	}
	if _, err := pool.Take(ctx, "sub-9", "sub-9", 0); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("taking from a mailbox that is not there: %v", err)
	}
	// A sub agent may take its own, and the main core may take anyone's.
	if _, err := pool.Take(ctx, second.ID, second.ID, 0); err != nil {
		t.Fatalf("a sub agent could not read its own mail: %v", err)
	}
	if _, err := pool.Take(ctx, "main", second.ID, 0); err != nil {
		t.Fatalf("the main core could not collect a result: %v", err)
	}
	if _, err := pool.Take(ctx, "main", second.ID, -1); err == nil {
		t.Fatal("a limit that cannot be read was accepted")
	}
	// Asking for more messages than exist returns what there is. A caller that asked
	// for ten and there are three must not be left sitting on the fourth for ever.
	if _, err := broker.Send(ctx, Message{
		FromAgentID: "main", ToAgentID: first.ID, Type: "task", Content: "one",
	}); err != nil {
		t.Fatal(err)
	}
	taken, err := pool.Take(ctx, "main", first.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 {
		t.Fatalf("took %d messages when one was there", len(taken))
	}
}

// Asking what is going on must not take the answer out of the mailbox. A view that
// consumed would leave the second question finding nothing and the sub agent looking
// idle when it is merely unheard.
func TestLookingAtTheStateDoesNotConsumeAnything(t *testing.T) {
	pool, _, broker := newTestPool(t, &recordingBuilder{}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"first", "second", "third"} {
		if _, err := broker.Send(context.Background(), Message{
			FromAgentID: "main", ToAgentID: slot.ID, Type: "task", Content: content,
		}); err != nil {
			t.Fatal(err)
		}
	}
	view := pool.View()
	if len(view.SubAgents) != 1 {
		t.Fatalf("the view holds %d sub agents", len(view.SubAgents))
	}
	entry := view.SubAgents[0]
	if entry.Waiting != 3 {
		t.Fatalf("the view says %d messages are waiting", entry.Waiting)
	}
	if entry.Delivered != 3 {
		t.Fatalf("the view says %d were delivered", entry.Delivered)
	}
	// Looking twice costs nothing and loses nothing.
	if again := pool.View(); again.SubAgents[0].Waiting != 3 {
		t.Fatalf("a second look found %d waiting", again.SubAgents[0].Waiting)
	}
	// And the messages are all still there to be taken.
	taken, err := pool.Take(context.Background(), "main", slot.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 3 {
		t.Fatalf("took %d of 3", len(taken))
	}
	if got := pool.View(); got.SubAgents[0].Waiting != 0 {
		t.Fatalf("the view still says %d are waiting", got.SubAgents[0].Waiting)
	}
}

// The view is one record of the whole truth, so a main core reading it gets the live
// state, the durable state, what the sub agent is confined to and what went wrong,
// without having to ask three places and reconcile them.
func TestTheViewGathersWhatThePoolAndTheRegistryEachKnow(t *testing.T) {
	pool, _, broker := newTestPool(t, &recordingBuilder{}, 2, 2)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: slot.ID, Type: "task", Content: "look",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runTaskOn(pool, slot.ID, "look"); err != nil {
		t.Fatal(err)
	}
	view := pool.View()
	entry, ok := view.Get(slot.ID)
	if !ok {
		t.Fatal("the view does not hold the sub agent")
	}
	// Live, from the pool.
	if entry.State != SlotIdle {
		t.Fatalf("the live state is %q", entry.State)
	}
	if entry.Runs != 1 {
		t.Fatalf("the view says %d runs", entry.Runs)
	}
	// Durable, from the registry.
	if entry.Recorded != StateDone {
		t.Fatalf("the recorded state is %q", entry.Recorded)
	}
	// Confined, from the template the registry filed.
	if len(entry.Tools) == 0 {
		t.Fatal("the view does not say what the sub agent may use")
	}
	// What it was asked and what it made of it.
	if entry.Summary != "asked to look" {
		t.Fatalf("the summary is %q", entry.Summary)
	}
	if entry.Template != "reviewer" {
		t.Fatalf("the template is %q", entry.Template)
	}
	// The main core is not in the list of its own sub agents.
	for _, agent := range view.SubAgents {
		if agent.ID == "main" {
			t.Fatal("the main core listed itself as a sub agent")
		}
	}
	if view.Capacity != 2 {
		t.Fatalf("the capacity is %d", view.Capacity)
	}
	if got := view.Summary()[SlotIdle]; got != 1 {
		t.Fatalf("the counts say %d idle", got)
	}
}

// A sub agent with nothing to do waits for work, and a waiter that is given a
// deadline is let go rather than left for good.
func TestASubAgentCanWaitForWorkAndBeInterrupted(t *testing.T) {
	pool, _, broker := newTestPool(t, &recordingBuilder{}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.WaitMessages(ctx, slot.ID, 0); err == nil {
		t.Fatal("a sub agent with no message waiting was given one")
	}
	if _, err := pool.WaitMessages(ctx, "no-such-slot", time.Millisecond); !errors.Is(err, ErrNoSlot) {
		t.Fatalf("waiting on a mailbox that is not there: %v", err)
	}
	// A message arriving while nobody is waiting is still there afterwards.
	if _, err := broker.Send(ctx, Message{
		FromAgentID: "main", ToAgentID: slot.ID, Type: "task", Content: "work",
	}); err != nil {
		t.Fatal(err)
	}
	message, err := pool.WaitMessages(ctx, slot.ID, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "work" {
		t.Fatalf("the message says %q", message.Content)
	}
	if _, err := pool.WaitMessages(ctx, slot.ID, 20*time.Millisecond); err == nil {
		t.Fatal("a wait with nothing coming succeeded")
	}
	shortCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := pool.WaitMessages(shortCtx, slot.ID, time.Hour); err == nil {
		t.Fatal("a wait that ran out succeeded")
	}
}

// The tools are built for the main core and refuse to be built for anyone else, so a
// misconfigured assembly cannot hand a sub agent the power to make more of itself.
func TestTheToolsAreBuiltForTheMainCoreOrNotAtAll(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	if _, err := NewOrchestrator(OrchestrationConfig{MainID: "main"}); err == nil {
		t.Fatal("the tools were built with no pool")
	}
	if _, err := NewOrchestrator(OrchestrationConfig{Pool: pool}); err == nil {
		t.Fatal("the tools were built with no main core")
	}
	if _, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "sub-1"}); err == nil {
		t.Fatal("the tools were built to act as a sub agent")
	}
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// The names are stable, because they go into a cached prompt and a set that
	// reordered itself between turns would throw that cache away every time.
	first := strings.Join(orchestrator.Names(), ",")
	for range 5 {
		if again := strings.Join(orchestrator.Names(), ","); again != first {
			t.Fatalf("the tool order changed: %q then %q", first, again)
		}
	}
	specs := orchestrator.Specs()
	if len(specs) != len(orchestrator.Tools()) {
		t.Fatalf("%d tools but %d specs", len(orchestrator.Tools()), len(specs))
	}
	for _, spec := range specs {
		if spec.Name == "" || spec.Description == "" {
			t.Fatalf("the spec for %q is incomplete", spec.Name)
		}
		if !json.Valid(spec.Parameters) {
			t.Fatalf("the schema for %q is not valid JSON", spec.Name)
		}
	}
}

// A tool's schema is copied out, so a caller that keeps a reference cannot change the
// arguments out from under every later turn.
func TestASchemasSchemaCannotBeChangedFromOutside(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range orchestrator.Tools() {
		schema := tool.Parameters()
		before := string(schema)
		for index := range schema {
			schema[index] = ' '
		}
		if again := string(tool.Parameters()); again != before {
			t.Fatalf("the schema for %q was changed through the copy it handed out", tool.Name())
		}
	}
}

// Sending a message to a running sub agent is how a main core redirects work it did
// not foresee, so it goes through the journal like every other message.
func TestAMessageReachesARunningSubAgentThroughTheJournal(t *testing.T) {
	pool, _, broker := newTestPool(t, &recordingBuilder{}, 1, 1)
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	var send core.Tool
	for _, tool := range orchestrator.Tools() {
		if tool.Name() == "send_subagent_message" {
			send = tool
		}
	}
	sent := run(t, send, `{"to":"`+slot.ID+`","content":"stop and check the tests first"}`)
	if sent.IsError {
		t.Fatalf("sending failed: %s", sent.Error)
	}
	if !hasField(t, sent, "seq") {
		t.Fatal("the message was not given a position in the journal")
	}
	// It was written before it was delivered, which is the whole point of the broker,
	// and it arrived exactly once.
	mailbox, ok := broker.Mailbox(slot.ID)
	if !ok {
		t.Fatal("the sub agent has no mailbox")
	}
	message, got := mailbox.ReceiveWithin(time.Second)
	if !got {
		t.Fatal("the message did not arrive")
	}
	if message.Content != "stop and check the tests first" {
		t.Fatalf("the message says %q", message.Content)
	}
	if message.Type != "task" {
		t.Fatalf("the message is a %q", message.Type)
	}
	if _, again := mailbox.ReceiveWithin(20 * time.Millisecond); again {
		t.Fatal("the message arrived twice")
	}
}

// A pool with no broker cannot carry messages, and the tool that sends them says so
// rather than appearing to succeed.
func TestSendingWithNoBrokerIsRefused(t *testing.T) {
	registry, err := NewRegistry(RegistryConfig{MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(PooledAgentConfig{
		MainID: "main", Registry: registry,
		Templates: templatesFor(t), Builder: &recordingBuilder{}, Capacity: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Take(ctx, "main", "slot-1", 0); err == nil {
		t.Fatal("taking from a pool with no broker worked")
	}
	if _, err := pool.WaitMessages(ctx, "slot-1", 0); err == nil {
		t.Fatal("waiting on a pool with no broker worked")
	}
	for _, tool := range orchestrator.Tools() {
		if tool.Name() != "send_subagent_message" && tool.Name() != "read_subagent_messages" {
			continue
		}
		result, err := tool.Execute(ctx, json.RawMessage(`{"to":"slot-1","content":"x"}`))
		if err != nil {
			t.Fatalf("%s failed rather than refusing: %v", tool.Name(), err)
		}
		if !result.IsError {
			t.Fatalf("%s appeared to succeed with no broker", tool.Name())
		}
	}
}

// Reading a result for work still running must say the work is running, and must not
// report a failure. A model told "failed" would go looking for a fault that is not
// there, in a sub agent that is doing exactly what it was asked.
func TestReadingARunningSubAgentSaysPendingRatherThanFailed(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{delay: 300 * time.Millisecond}, 1, 1)
	orchestrator, err := NewOrchestrator(OrchestrationConfig{
		Pool: pool, MainID: "main", WaitFor: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Start(context.Background(), "main", Task{
		Slot: slot.ID, Input: core.Input{Type: core.InputTypeText, Content: "slow"},
	}); err != nil {
		t.Fatal(err)
	}
	var read core.Tool
	for _, tool := range orchestrator.Tools() {
		if tool.Name() == "read_subagent_result" {
			read = tool
		}
	}
	result := run(t, read, `{"id":"`+slot.ID+`"}`)
	if result.IsError {
		t.Fatalf("reading a running sub agent was a refusal: %s", result.Error)
	}
	if !boolField(t, result, "pending") {
		t.Fatalf("the answer does not say the work is pending: %s", result.Output)
	}
}

// A task with no summary of its own gets one from its first line, so a sub agent is
// never left with nothing written down about what it was for. A task in a language
// whose characters are multi-byte is not cut through the middle of one.
func TestASummaryIsMadeFromTheTaskAndSurvivesAwkwardText(t *testing.T) {
	cases := []struct{ task, says string }{
		{"read the file and report", "read the file and report"},
		{"first line\nsecond line", "first line"},
		{"   ", "an unnamed task"},
		{"", "an unnamed task"},
	}
	for _, testCase := range cases {
		if got := firstLine(testCase.task); got != testCase.says {
			t.Fatalf("firstLine(%q) is %q, not %q", testCase.task, got, testCase.says)
		}
	}
	// A long line of multi-byte characters is cut on a character boundary, so the
	// summary never holds half a character.
	long := strings.Repeat("检查文件内容", 60)
	got := firstLine(long)
	if !utf8.ValidString(got) {
		t.Fatalf("the summary is not valid text: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("a long summary was not shortened: %q", got)
	}
	// Every tool that makes a summary of its own uses this one, so the rule is
	// checked where it is applied.
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); err != nil {
		t.Fatal(err)
	}
	var start core.Tool
	for _, tool := range orchestrator.Tools() {
		if tool.Name() == "start_subagent" {
			start = tool
		}
	}
	run(t, start, `{"task":"do the thing\nand also the other"}`)
	if _, err := pool.Wait(context.Background(), "slot-1"); err != nil {
		t.Fatal(err)
	}
	view := pool.View()
	if got := view.SubAgents[0].Summary; got != "do the thing" {
		t.Fatalf("the summary is %q", got)
	}
}

// A tool call is a promise about what will happen, so a start that is refused must
// leave nothing behind, and a start that is accepted must be reflected in the view
// before the tool returns.
func TestStartingThroughAToolIsVisibleBeforeItReturns(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{delay: 100 * time.Millisecond}, 1, 1)
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); err != nil {
		t.Fatal(err)
	}
	var start core.Tool
	for _, tool := range orchestrator.Tools() {
		if tool.Name() == "start_subagent" {
			start = tool
		}
	}
	run(t, start, `{"task":"work"}`)
	// No waiting, no sleeping: the tool said the sub agent was busy, so by the time
	// it returned it must be busy. A tool that reported otherwise before the work
	// began would be reporting a hope.
	if got := pool.View().SubAgents[0].State; got != SlotBusy {
		t.Fatalf("the sub agent is %q after being started", got)
	}
	if _, err := pool.Wait(context.Background(), "slot-1"); err != nil {
		t.Fatal(err)
	}
}

// The tools answer a caller that has gone away, because a turn that was cancelled
// should not leave a tool call running against a dead context.
func TestTheToolsAnswerACancelledCaller(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	orchestrator, err := NewOrchestrator(OrchestrationConfig{Pool: pool, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A read costs nothing and changes nothing, so a read is allowed to answer a
	// caller that has gone. Anything that acts must refuse, because a sub agent
	// created for a turn that was abandoned is a sub agent nobody is looking after.
	readsOnly := map[string]bool{
		"list_subagents": true, "read_subagent_result": true, "read_subagent_messages": true,
	}
	for _, tool := range orchestrator.Tools() {
		result, err := tool.Execute(ctx, json.RawMessage(
			`{"template":"reviewer","id":"slot-1","to":"slot-1","content":"x","task":"x"}`))
		if readsOnly[tool.Name()] {
			continue
		}
		if err == nil && !result.IsError {
			t.Fatalf("%s acted on a cancelled context", tool.Name())
		}
	}
	if got := pool.View(); len(got.SubAgents) != 0 {
		t.Fatalf("a cancelled caller left %d sub agents behind", len(got.SubAgents))
	}
}

// run calls a tool and fails the test if the tool itself faulted, since a fault is a
// bug in the tool rather than an answer it meant to give.
func run(t *testing.T, tool core.Tool, call string) core.ToolResult {
	t.Helper()
	if tool == nil {
		t.Fatal("the tool is not there")
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(call))
	if err != nil {
		t.Fatalf("%s(%s) faulted: %v", tool.Name(), call, err)
	}
	return result
}

// hasField reports whether an answer carries a field at all, whatever its type.
func hasField(t *testing.T, result core.ToolResult, name string) bool {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("the answer is not an object: %s", result.Output)
	}
	_, ok := decoded[name]
	return ok
}

// boolField reads a boolean out of a tool's answer.
func boolField(t *testing.T, result core.ToolResult, name string) bool {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("the answer is not an object: %s", result.Output)
	}
	value, ok := decoded[name].(bool)
	if !ok {
		t.Fatalf("the answer has no boolean %q: %s", name, result.Output)
	}
	return value
}

// field reads a string out of a tool's answer.
func field(t *testing.T, result core.ToolResult, name string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(result.Output, &decoded); err != nil {
		t.Fatalf("the answer is not an object: %s", result.Output)
	}
	value, ok := decoded[name].(string)
	if !ok {
		t.Fatalf("the answer has no %q: %s", name, result.Output)
	}
	return value
}
