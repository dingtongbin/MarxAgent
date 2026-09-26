// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"errors"
	"testing"
)

// A stage that cannot be promised must not run. The journal is what makes a sub
// agent's work auditable, so a stage whose task was never written is a stage that
// must not happen, and these tests are the proof rather than the intention.

func TestAStageIsNotRunWhenTheJournalRefusesIt(t *testing.T) {
	sentinel := errors.New("the disk said no")
	broker := newTestBroker(t, &recordingJournal{failWrite: sentinel})
	registry := newTestRegistry(t, &stubSpawner{state: StateRunning}, broker)
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	run := func(context.Context, string) (any, error) {
		ran++
		return "should not happen", nil
	}
	if _, err := orchestration.Sequential(context.Background(), []Spec{{Name: "a"}}, run); !errors.Is(err, sentinel) {
		t.Fatalf("sequential: err = %v", err)
	}
	if ran != 0 {
		t.Fatalf("sequential: the stage ran %d times", ran)
	}
	if _, err := orchestration.Parallel(context.Background(), []Spec{{Name: "a"}}, run); err == nil {
		t.Fatal("parallel: the failure was not reported")
	}
	if ran != 0 {
		t.Fatalf("parallel: the stage ran %d times", ran)
	}
	pipeline := func(context.Context, string, any) (any, error) {
		ran++
		return "should not happen", nil
	}
	if _, err := orchestration.Pipeline(context.Background(), []Spec{{Name: "a"}}, pipeline); !errors.Is(err, sentinel) {
		t.Fatalf("pipeline: err = %v", err)
	}
	if ran != 0 {
		t.Fatalf("pipeline: the stage ran %d times", ran)
	}
	// The agent that could not be promised is not left marked as running.
	for _, agent := range registry.List() {
		if agent.Name == "a" && agent.State == StateRunning {
			t.Fatalf("an unpromised agent was left running: %#v", agent)
		}
	}
}

func TestEncodeInputDropsWhatItCannotEncode(t *testing.T) {
	// The journaled input is a convenience for the audit trail, not the delivery
	// mechanism, so an unencodable value is dropped rather than failing the stage.
	original := jsonMarshal
	defer func() { jsonMarshal = original }()
	jsonMarshal = func(any) ([]byte, error) { return nil, errors.New("cannot encode") }
	if got := encodeInput(struct{ A int }{A: 1}); got != nil {
		t.Fatalf("encodeInput = %s", got)
	}
	if got := encodeInput(nil); got != nil {
		t.Fatalf("encodeInput(nil) = %s", got)
	}
	jsonMarshal = original
	if got := encodeInput(map[string]int{"a": 1}); string(got) != `{"a":1}` {
		t.Fatalf("encodeInput = %s", got)
	}
}

func TestRegistryFinishReportsAnUnknownAgent(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{}, nil)
	// A composition whose agent vanished calls Finish on an identifier the registry
	// no longer holds, and that has to be reported rather than panicking.
	if err := registry.Finish("ghost", errors.New("x")); !errors.Is(err, ErrNoSuchAgent) {
		t.Fatalf("err = %v", err)
	}
}

func TestSequentialReportsASpawnFailure(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{failWith: errors.New("no capacity")}, nil)
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	results, err := orchestration.Sequential(context.Background(),
		[]Spec{{Name: "a"}},
		func(context.Context, string) (any, error) { return "ok", nil })
	if err == nil {
		t.Fatal("a spawn failure was not reported")
	}
	if len(results) != 0 {
		t.Fatalf("results = %#v", results)
	}
	if _, err := orchestration.Parallel(context.Background(),
		[]Spec{{Name: "a"}},
		func(context.Context, string) (any, error) { return "ok", nil }); err == nil {
		t.Fatal("a spawn failure was not reported by the parallel form")
	}
	if _, err := orchestration.Pipeline(context.Background(),
		[]Spec{{Name: "a"}},
		func(context.Context, string, any) (any, error) { return "ok", nil }); err == nil {
		t.Fatal("a spawn failure was not reported by the pipeline form")
	}
}

func TestPipelineStopsOnACancelledContext(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{state: StateRunning}, newTestBroker(t, &recordingJournal{}))
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := 0
	run := func(context.Context, string, any) (any, error) {
		ran++
		cancel()
		return "ok", nil
	}
	// The first stage runs and cancels; the second must not.
	results, err := orchestration.Pipeline(ctx,
		[]Spec{{Name: "a"}, {Name: "b"}, {Name: "c"}}, run)
	if err == nil {
		t.Fatal("a cancelled pipeline reported success")
	}
	if ran != 1 {
		t.Fatalf("stages ran = %d", ran)
	}
	if len(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
}

func TestAStageThatReturnsNothingStillSucceeds(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{state: StateRunning}, newTestBroker(t, &recordingJournal{}))
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	// An agent that produces no output is not a failure. A stage is judged by its
	// error, not by whether it had anything to say.
	results, err := orchestration.Sequential(context.Background(), []Spec{{Name: "a"}},
		func(context.Context, string) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Succeeded() {
		t.Fatalf("results = %#v", results)
	}
}

func TestRegistryRemembersTheAgentItSpawned(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	spawner := &stubSpawner{state: StateStarting}
	registry := newTestRegistry(t, spawner, broker)
	agent, err := registry.Spawn(context.Background(), "main", Spec{
		Name: "researcher", SystemPrompt: "you research things",
		Metadata: map[string]any{"budget": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The state the handle reported is the state recorded, so a caller reading the
	// registry sees what the agent actually said about itself.
	stored, _ := registry.Get(agent.ID)
	if stored.State != StateStarting {
		t.Fatalf("state = %q", stored.State)
	}
	if stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
		t.Fatalf("timestamps = %v %v", stored.CreatedAt, stored.UpdatedAt)
	}
	// The spec reached the spawner intact, including the prompt that keeps a sub
	// agent from inheriting its parent's.
	specs := spawner.specs()
	if specs[0].SystemPrompt != "you research things" {
		t.Fatalf("system prompt = %q", specs[0].SystemPrompt)
	}
	if specs[0].ParentID != "main" {
		t.Fatalf("parent = %q", specs[0].ParentID)
	}
	// A finished agent releases its broker mailbox, because an agent that stopped
	// has nothing to receive.
	if err := registry.Finish(agent.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := broker.Mailbox(agent.ID); exists {
		t.Fatal("a finished agent kept its mailbox")
	}
	// The main core keeps its own mailbox, which is how a sub agent reports back.
	if _, exists := broker.Mailbox("main"); !exists {
		t.Fatal("the main core lost its mailbox")
	}
}
