// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubSpawner creates handles that do nothing, so the registry's rules can be
// tested without building a real agent.
type stubSpawner struct {
	mu       sync.Mutex
	spawned  []Spec
	failWith error
	// state is what a handle reports.
	state State
	// idOffset makes each handle's identifier distinct.
	idOffset int
}

func (s *stubSpawner) Spawn(_ context.Context, spec Spec) (Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return nil, s.failWith
	}
	s.spawned = append(s.spawned, spec)
	return &stubHandle{id: spec.AgentID, state: s.state}, nil
}

func (s *stubSpawner) specs() []Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Spec, len(s.spawned))
	copy(out, s.spawned)
	return out
}

type stubHandle struct {
	id    string
	state State
	err   error
	mu    sync.Mutex
	ran   bool
}

func (h *stubHandle) AgentID() string { return h.id }
func (h *stubHandle) Run(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = true
	return h.err
}
func (h *stubHandle) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state
}

func newTestRegistry(t *testing.T, spawner Spawner, broker *Broker) *Registry {
	t.Helper()
	registry, err := NewRegistry(RegistryConfig{
		Spawner: spawner, Broker: broker, MainID: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Shutdown)
	return registry
}

func TestNewRegistryNeedsTheMainCore(t *testing.T) {
	if _, err := NewRegistry(RegistryConfig{}); err == nil {
		t.Fatal("a registry without a main core identifier was accepted")
	}
	registry, err := NewRegistry(RegistryConfig{MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	registry.Shutdown()
	// A mode with no sub agents is a real configuration, so a nil broker is
	// allowed and a nil spawner refuses rather than silently doing nothing.
	if registry.Broker() != nil {
		t.Fatal("a registry with no broker reported one")
	}
	if _, err := registry.Spawn(context.Background(), "main", Spec{Name: "x"}); err == nil {
		t.Fatal("a spawn with no spawner was accepted")
	}
}

func TestOnlyTheMainCoreMaySpawn(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	registry := newTestRegistry(t, spawner, nil)
	agent, err := registry.Spawn(context.Background(), "main", Spec{Name: "researcher"})
	if err != nil {
		t.Fatal(err)
	}
	if agent.ParentID != "main" {
		t.Fatalf("parent = %q", agent.ParentID)
	}
	// The rule the design states: a sub agent may not create another. A chain would
	// multiply work no one bounded and an audit trail no one could follow.
	_, err = registry.Spawn(context.Background(), agent.ID, Spec{Name: "nested"})
	if !errors.Is(err, ErrNotMain) {
		t.Fatalf("err = %v", err)
	}
	// An identifier nobody registered is refused the same way, rather than being
	// trusted because it looks like an agent.
	_, err = registry.Spawn(context.Background(), "impostor", Spec{Name: "nested"})
	if !errors.Is(err, ErrNotMain) {
		t.Fatalf("err = %v", err)
	}
	if len(spawner.specs()) != 1 {
		t.Fatalf("spawned = %d", len(spawner.specs()))
	}
}

func TestSpawnGivesTheAgentItsOwnIdentity(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	registry := newTestRegistry(t, spawner, nil)
	agent, err := registry.Spawn(context.Background(), "main", Spec{
		Name: "researcher", Tools: []string{"read", "grep"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if agent.ID == "" {
		t.Fatal("the agent has no identifier")
	}
	if agent.IsMain() {
		t.Fatal("a sub agent reported itself as the main core")
	}
	// The tool list is copied, so a caller that reuses its slice cannot change what
	// the agent is allowed afterwards.
	specs := spawner.specs()
	if len(specs[0].Tools) != 2 {
		t.Fatalf("tools = %v", specs[0].Tools)
	}
	specs[0].Tools[0] = "bash"
	stored, _ := registry.Get(agent.ID)
	if stored.Tools[0] == "bash" {
		t.Fatal("the agent's tool list aliases the caller's slice")
	}
	main, _ := registry.Get("main")
	if !main.IsMain() {
		t.Fatal("the main core is not recognised as the main core")
	}
}

func TestSpawnRefusesDuplicatesAndUnboundedGrowth(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	registry := newTestRegistry(t, spawner, nil)
	if _, err := registry.Spawn(context.Background(), "main", Spec{Name: "a", AgentID: "fixed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Spawn(context.Background(), "main", Spec{Name: "b", AgentID: "fixed"}); err == nil {
		t.Fatal("a duplicate identifier was accepted")
	}
	// A bound is a real defence against a parent that does not stop.
	bounded := newTestRegistry(t, spawner, nil)
	bounded.maxAgents = 2
	for index := 0; index < 5; index++ {
		_, err := bounded.Spawn(context.Background(), "main", Spec{Name: fmt.Sprint(index)})
		if index < 1 {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("spawn %d exceeded the bound", index)
		}
		if !strings.Contains(err.Error(), "maximum") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestSpawnReportsASpawnerFailure(t *testing.T) {
	sentinel := errors.New("no capacity for another agent")
	spawner := &stubSpawner{failWith: sentinel}
	registry := newTestRegistry(t, spawner, nil)
	_, err := registry.Spawn(context.Background(), "main", Spec{Name: "a"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	// The agent was not registered, so nothing is left half created.
	if len(registry.List()) != 1 {
		t.Fatalf("agents = %d", len(registry.List()))
	}
}

func TestSpawnRegistersABrokerMailbox(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	spawner := &stubSpawner{state: StateRunning}
	registry := newTestRegistry(t, spawner, broker)
	agent, err := registry.Spawn(context.Background(), "main", Spec{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := broker.Mailbox(agent.ID); !exists {
		t.Fatal("the agent has no mailbox")
	}
	// A mailbox that was registered for a spawn that then failed is released, so a
	// failed spawn does not leak a name.
	failing := &stubSpawner{failWith: errors.New("no")}
	failingRegistry := newTestRegistry(t, failing, newTestBroker(t, &recordingJournal{}))
	before := len(failingRegistry.Broker().Agents())
	if _, err := failingRegistry.Spawn(context.Background(), "main", Spec{Name: "a"}); err == nil {
		t.Fatal("the spawn succeeded")
	}
	if len(failingRegistry.Broker().Agents()) != before {
		t.Fatal("a failed spawn left a mailbox behind")
	}
}

func TestRegistryRecordsWhatHappenedToAnAgent(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	registry := newTestRegistry(t, spawner, nil)
	agent, err := registry.Spawn(context.Background(), "main", Spec{Name: "researcher"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := registry.Handle(agent.ID); !exists {
		t.Fatal("no handle was kept")
	}
	if err := registry.SetState(agent.ID, StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := registry.NoteMessage(agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := registry.NoteMessage(agent.ID); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetSummary(agent.ID, "found three things"); err != nil {
		t.Fatal(err)
	}
	stored, _ := registry.Get(agent.ID)
	if stored.State != StateRunning || stored.MessageCount != 2 {
		t.Fatalf("agent = %#v", stored)
	}
	if stored.Summary != "found three things" {
		t.Fatalf("summary = %q", stored.Summary)
	}
	// A finished agent releases its handle, because there is nothing left to drive.
	if err := registry.Finish(agent.ID, nil); err != nil {
		t.Fatal(err)
	}
	stored, _ = registry.Get(agent.ID)
	if stored.State != StateDone {
		t.Fatalf("state = %q", stored.State)
	}
	if _, exists := registry.Handle(agent.ID); exists {
		t.Fatal("a finished agent still has a handle")
	}
}

func TestRegistryRecordsWhyAnAgentFailed(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	registry := newTestRegistry(t, spawner, nil)
	agent, err := registry.Spawn(context.Background(), "main", Spec{Name: "researcher"})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("the tool call was refused")
	if err := registry.Finish(agent.ID, sentinel); err != nil {
		t.Fatal(err)
	}
	stored, _ := registry.Get(agent.ID)
	if stored.State != StateFailed {
		t.Fatalf("state = %q", stored.State)
	}
	if !strings.Contains(stored.Err, "refused") {
		t.Fatalf("err = %q", stored.Err)
	}
}

func TestRegistryRefusesOperationsOnAnUnknownAgent(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{}, nil)
	for name, err := range map[string]error{
		"set state":   registry.SetState("ghost", StateRunning),
		"set summary": registry.SetSummary("ghost", "x"),
		"note":        registry.NoteMessage("ghost"),
		"finish":      registry.Finish("ghost", nil),
	} {
		if !errors.Is(err, ErrNoSuchAgent) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestSequentialStopsAtTheFirstFailure(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	broker := newTestBroker(t, &recordingJournal{})
	registry := newTestRegistry(t, spawner, broker)
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("the second stage had no data")
	specs := []Spec{{Name: "one"}, {Name: "two"}, {Name: "three"}}
	results, err := orchestration.Sequential(context.Background(), specs,
		func(_ context.Context, agentID string) (any, error) {
			if agentID == "sub-2" {
				return nil, sentinel
			}
			return "done", nil
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	// Stopping is the point: a later stage depending on the failed one would work
	// from nothing, and running it anyway produces a result that looks complete.
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	if !results[0].Succeeded() {
		t.Fatalf("the first stage did not succeed: %#v", results[0])
	}
	if results[1].Succeeded() {
		t.Fatalf("the failed stage reported success: %#v", results[1])
	}
	if !errors.Is(err, ErrOrchestration) {
		t.Fatal("the failure was not reported as a composition failure")
	}
	// Each stage was promised in the journal before it ran.
	if broker.Stats().Sent != 2 {
		t.Fatalf("stats = %#v", broker.Stats())
	}
}

func TestSequentialHappensInOrder(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	registry := newTestRegistry(t, spawner, newTestBroker(t, &recordingJournal{}))
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	results, err := orchestration.Sequential(context.Background(),
		[]Spec{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		func(_ context.Context, _ string) (any, error) {
			order = append(order, "ran")
			return "ok", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || len(results) != 3 {
		t.Fatalf("order = %v results = %d", order, len(results))
	}
	for index, result := range results {
		if !result.Succeeded() || result.Output != "ok" {
			t.Fatalf("result %d = %#v", index, result)
		}
		// A finished agent carries a summary, which is what another agent sees when
		// it asks about this one.
		stored, _ := registry.Get(result.AgentID)
		if stored.Summary != "ok" {
			t.Fatalf("summary = %q", stored.Summary)
		}
	}
}

func TestParallelRunsEveryAgentEvenAfterOneFails(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	registry := newTestRegistry(t, spawner, newTestBroker(t, &recordingJournal{}))
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("one question had no answer")
	specs := []Spec{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	results, err := orchestration.Parallel(context.Background(), specs,
		func(_ context.Context, agentID string) (any, error) {
			if agentID == "sub-2" {
				return nil, sentinel
			}
			return "answered", nil
		})
	if err == nil {
		t.Fatal("a failure was not reported")
	}
	// The results come back alongside the error, because a caller usually wants
	// the ones that worked as much as the one that did not.
	if len(results) != 3 {
		t.Fatalf("results = %d", len(results))
	}
	succeeded := 0
	for _, result := range results {
		if result.Succeeded() {
			succeeded++
		}
	}
	if succeeded != 2 {
		t.Fatalf("succeeded = %d", succeeded)
	}
	// A fan out is a set of independent questions, so one failure does not cancel
	// the work that had already been paid for.
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Fatalf("err = %v", err)
	}
}

func TestParallelWithNothingToDo(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{}, newTestBroker(t, &recordingJournal{}))
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	results, err := orchestration.Parallel(context.Background(), nil,
		func(context.Context, string) (any, error) { return nil, nil })
	if err != nil || results != nil {
		t.Fatalf("results = %#v err = %v", results, err)
	}
}

func TestPipelineThreadsOutputIntoTheNextStage(t *testing.T) {
	spawner := &stubSpawner{state: StateRunning}
	broker := newTestBroker(t, &recordingJournal{})
	registry := newTestRegistry(t, spawner, broker)
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	var seen []any
	results, err := orchestration.Pipeline(context.Background(),
		[]Spec{{Name: "gather"}, {Name: "summarise"}, {Name: "format"}},
		func(_ context.Context, _ string, input any) (any, error) {
			seen = append(seen, input)
			return fmt.Sprintf("step %d", len(seen)), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d", len(results))
	}
	// The stages run one after another, because a pipeline is a chain: each stage
	// transforms what the previous one produced.
	if len(seen) != 3 || seen[0] != nil || seen[1] != "step 1" || seen[2] != "step 2" {
		t.Fatalf("inputs = %#v", seen)
	}
	// Each stage's input was journaled alongside the task, which is what makes the
	// chain auditable after the fact.
	if broker.Stats().Sent != 3 {
		t.Fatalf("stats = %#v", broker.Stats())
	}
}

func TestPipelineStopsAtAFailedStage(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{}, newTestBroker(t, &recordingJournal{}))
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	results, err := orchestration.Pipeline(context.Background(),
		[]Spec{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		func(context.Context, string, any) (any, error) {
			ran++
			if ran == 2 {
				return nil, errors.New("stage two failed")
			}
			return "ok", nil
		})
	if err == nil {
		t.Fatal("a failed stage was not reported")
	}
	if len(results) != 2 || ran != 2 {
		t.Fatalf("results = %d ran = %d", len(results), ran)
	}
}

func TestOrchestrationRefusesAnEmptyRun(t *testing.T) {
	if _, err := NewOrchestration(nil); err == nil {
		t.Fatal("an orchestration without a registry was accepted")
	}
	registry := newTestRegistry(t, &stubSpawner{}, nil)
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing to run is a wiring mistake, not an empty composition.
	if _, err := orchestration.Sequential(context.Background(), nil, nil); err == nil {
		t.Fatal("a composition with nothing to run was accepted")
	}
	if _, err := orchestration.Parallel(context.Background(), nil, nil); err == nil {
		t.Fatal("a composition with nothing to run was accepted")
	}
	if _, err := orchestration.Pipeline(context.Background(), nil, nil); err == nil {
		t.Fatal("a composition with nothing to run was accepted")
	}
	// An empty stage list is legitimate and produces nothing.
	results, err := orchestration.Pipeline(context.Background(), nil,
		func(context.Context, string, any) (any, error) { return nil, nil })
	if err != nil || results != nil {
		t.Fatalf("results = %#v err = %v", results, err)
	}
}

func TestOrchestrationHonoursACancelledContext(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{}, newTestBroker(t, &recordingJournal{}))
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = orchestration.Sequential(ctx, []Spec{{Name: "a"}},
		func(context.Context, string) (any, error) { return "ok", nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	// A stage that cannot be promised is not run: the journal is what makes a sub
	// agent's work auditable, so a stage with no record does not happen.
	if _, err := orchestration.Pipeline(ctx, []Spec{{Name: "a"}},
		func(context.Context, string, any) (any, error) { return "ok", nil }); err == nil {
		t.Fatal("a cancelled pipeline ran a stage")
	}
}

func TestOrchestrationRunsWithoutABroker(t *testing.T) {
	// A mode may have no broker, and a composition must still work: the stages
	// simply are not journaled.
	registry := newTestRegistry(t, &stubSpawner{state: StateRunning}, nil)
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	results, err := orchestration.Parallel(context.Background(),
		[]Spec{{Name: "a"}, {Name: "b"}},
		func(_ context.Context, _ string) (any, error) { return "ok", nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
}

func TestOrchestrationSummarisesALongOutput(t *testing.T) {
	registry := newTestRegistry(t, &stubSpawner{state: StateRunning}, nil)
	orchestration, err := NewOrchestration(registry)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 5000)
	results, err := orchestration.Sequential(context.Background(), []Spec{{Name: "a"}},
		func(context.Context, string) (any, error) { return long, nil })
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := registry.Get(results[0].AgentID)
	if len(stored.Summary) != 200 {
		t.Fatalf("the summary was not truncated: %d characters", len(stored.Summary))
	}
}

func TestTimeNowCanBeMadeDeterministic(t *testing.T) {
	// The variable exists so a test can pin the clock; this asserts the seam works
	// rather than leaving it untested.
	original := timeNow
	defer func() { timeNow = original }()
	fixed := time.Unix(1234567890, 0).UTC()
	timeNow = func() time.Time { return fixed }
	broker := newTestBroker(t, &recordingJournal{})
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	sent, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: "child", Type: TypeTask,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sent.Timestamp.Equal(fixed) {
		t.Fatalf("timestamp = %v", sent.Timestamp)
	}
}
