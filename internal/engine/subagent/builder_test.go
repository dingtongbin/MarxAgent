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

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// The rule the whole design turns on: a sub agent is confined to what its template
// asked for, and what it can never have is the power to direct sub agents. A template
// is a file, so this has to be enforced where the agent is built rather than assumed
// of whoever writes the file.
func TestATemplateCannotGiveASubAgentThePowerToDirectSubAgents(t *testing.T) {
	resolver, reserved := stubOrchestration()
	provider := &recordingProvider{}
	builder, err := NewCoreBuilder(BuilderConfig{
		Provider: provider, MainID: "main", Tools: resolver, Reserved: reserved,
		Defaults: BuilderDefaults{Model: "test-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, _, _ := newTestPoolWithBuilder(t, builder, 1, 1)
	// A template that asks for everything, including the tools that would let it make
	// more of itself and direct the ones already running.
	slot, err := pool.CreateSlot(context.Background(), "main", "greedy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Start(context.Background(), "main", Task{
		Slot: slot.ID, Input: core.Input{Type: core.InputTypeText, Content: "work"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Wait(context.Background(), slot.ID); err != nil {
		t.Fatal(err)
	}

	// The model was offered only the ordinary tools. This is checked against what
	// actually went into the request, because a rule that lives only in the tool
	// declaration is a rule nobody has to keep.
	reservedSet := nameSet(reserved)
	for _, name := range provider.toolNames() {
		if _, isReserved := reservedSet[name]; isReserved {
			t.Fatalf("a sub agent was offered %q", name)
		}
	}
	wantKept := []string{"glob", "grep", "read", "write"}
	if names := provider.toolNames(); strings.Join(names, ",") != strings.Join(wantKept, ",") {
		t.Fatalf("the sub agent was offered %v, want only the ordinary tools %v", names, wantKept)
	}

	// And the main core can see that the request was turned down, because a template
	// quietly reaching for something it may not have is worth knowing about.
	view := pool.View()
	if len(view.SubAgents) != 1 {
		t.Fatalf("the view holds %d sub agents", len(view.SubAgents))
	}
	// Eight reserved names, and the one the fixture invented that does not exist.
	refused := view.SubAgents[0].Refused
	if len(refused) != len(reserved)+1 {
		t.Fatalf("the view reports %v as refused, want the %d reserved and the one that does not exist",
			refused, len(reserved))
	}
	// A name that does not exist is refused as well as a reserved one, and both belong
	// in the list. What matters is that every reserved name is in it and that no
	// ordinary tool is.
	refusedSet := nameSet(refused)
	for _, name := range reserved {
		if _, wasRefused := refusedSet[name]; !wasRefused {
			t.Fatalf("the view does not report %q as refused", name)
		}
	}
	for _, name := range wantKept {
		if _, wasRefused := refusedSet[name]; wasRefused {
			t.Fatalf("the view reports the ordinary tool %q as refused", name)
		}
	}
}

// A tool a template names but that does not exist is dropped, because a request
// declaring a tool the caller cannot run would have the model call it and be refused.
func TestAToolThatDoesNotExistIsNotOffered(t *testing.T) {
	resolver, reserved := stubOrchestration()
	provider := &recordingProvider{}
	builder, err := NewCoreBuilder(BuilderConfig{
		Provider: provider, MainID: "main", Tools: resolver, Reserved: reserved,
		Defaults: BuilderDefaults{Model: "test-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, _, _ := newTestPoolWithBuilder(t, builder, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "hopeful")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Start(context.Background(), "main", Task{
		Slot: slot.ID, Input: core.Input{Type: core.InputTypeText, Content: "work"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Wait(context.Background(), slot.ID); err != nil {
		t.Fatal(err)
	}
	// This template asks for the same thirteen names as the others, so the four
	// ordinary tools still reach it. The point of this test is that a name which
	// cannot be resolved is dropped rather than offered and then refused at the call.
	if names := provider.toolNames(); strings.Join(names, ",") != "glob,grep,read,write" {
		t.Fatalf("the sub agent was offered %v", names)
	}
	refused := pool.View().SubAgents[0].Refused
	if len(refused) != len(reserved)+1 {
		t.Fatalf("the view reports %v as refused", refused)
	}
}

// The tool list goes into a cached prompt, so its order has to be the same every
// time. A set that reordered itself between turns would throw the cache away for
// nothing.
func TestTheToolsOfferedAreInAStableOrder(t *testing.T) {
	resolver, reserved := stubOrchestration()
	provider := &recordingProvider{}
	builder, err := NewCoreBuilder(BuilderConfig{
		Provider: provider, MainID: "main", Tools: resolver, Reserved: reserved,
		Defaults: BuilderDefaults{Model: "test-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, _, _ := newTestPoolWithBuilder(t, builder, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "many")
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for round := range 5 {
		if _, err := pool.Start(context.Background(), "main", Task{
			Slot: slot.ID, Input: core.Input{Type: core.InputTypeText, Content: "work"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Wait(context.Background(), slot.ID); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(provider.toolNames(), ",")
		if round == 0 {
			first = joined
			continue
		}
		if joined != first {
			t.Fatalf("round %d offered %q, the first round offered %q", round, joined, first)
		}
	}
	if first != "glob,grep,read,write" {
		t.Fatalf("the tools are not in name order: %q", first)
	}
	_ = slot
}

// Each task gets its own agent and its own conversation, so two tasks on one slot
// cannot see each other's history. This is checked against what reached the model,
// because that is the only place two contexts could have mixed.
func TestEachTaskGetsItsOwnAgentContextAndLoop(t *testing.T) {
	resolver, reserved := stubOrchestration()
	provider := &recordingProvider{}
	builder, err := NewCoreBuilder(BuilderConfig{
		Provider: provider, MainID: "main", Tools: resolver, Reserved: reserved,
		Defaults: BuilderDefaults{Model: "test-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, _, _ := newTestPoolWithBuilder(t, builder, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	for round := range 3 {
		if _, err := pool.Start(context.Background(), "main", Task{
			Slot:  slot.ID,
			Input: core.Input{Type: core.InputTypeText, Content: fmt.Sprintf("round %d", round)},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Wait(context.Background(), slot.ID); err != nil {
			t.Fatal(err)
		}
		// A fresh context holds this task's own message and nothing of any earlier
		// one. A carried-over transcript would be longer, and would still hold the
		// text of the rounds before it.
		messages, text := provider.lastRequest()
		if len(messages) != 1 {
			t.Fatalf("round %d was given %d messages, so the context was not its own",
				round, len(messages))
		}
		if !strings.Contains(text, fmt.Sprintf("round %d", round)) {
			t.Fatalf("round %d was given %q", round, text)
		}
	}
	// The agent itself is new each time, which is the reason the context can be. Two
	// tasks sharing one would put the first transcript in front of the second.
	first, err := builder.Build(context.Background(), Template{Name: "reviewer"}, &Slot{ID: "slot-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := builder.Build(context.Background(), Template{Name: "reviewer"}, &Slot{ID: "slot-1"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("the builder handed back the same agent twice")
	}
	// The sub agent knows who its parent is and that it may not delegate, so the
	// answer does not depend on it having read its tool list.
	view := first.State()
	if view.ParentID() != "main" {
		t.Fatalf("the parent is %q", view.ParentID())
	}
	if view.Metadata()["delegable"] != false {
		t.Fatalf("the agent does not say it may not delegate: %v", view.Metadata())
	}
}

// A template decides the model, the loop limit and the instructions. What it leaves
// out comes from the defaults, because a template that forgot to bound its own loop
// should not get to spend the main core's budget without limit.
func TestATemplateDecidesTheModelAndTheDefaultsFillTheGaps(t *testing.T) {
	resolver, reserved := stubOrchestration()
	provider := &recordingProvider{}
	builder, err := NewCoreBuilder(BuilderConfig{
		Provider: provider, MainID: "main", Tools: resolver, Reserved: reserved,
		Defaults: BuilderDefaults{
			Model: "default-model", MaxIterations: 7,
			SystemSuffix: "Answer in the language of the task.", SessionPrefix: "worker",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, _, _ := newTestPoolWithBuilder(t, builder, 2, 2)
	// The template names a model, so its choice stands.
	chosen, err := pool.CreateSlot(context.Background(), "main", "chosen")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Start(context.Background(), "main", Task{
		Slot: chosen.ID, Input: core.Input{Type: core.InputTypeText, Content: "work"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Wait(context.Background(), chosen.ID); err != nil {
		t.Fatal(err)
	}
	if got := provider.model(); got != "chosen-model" {
		t.Fatalf("the model was %q", got)
	}
	// The template names none, so the default stands.
	fallback, err := pool.CreateSlot(context.Background(), "main", "plain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Start(context.Background(), "main", Task{
		Slot: fallback.ID, Input: core.Input{Type: core.InputTypeText, Content: "work"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Wait(context.Background(), fallback.ID); err != nil {
		t.Fatal(err)
	}
	if got := provider.model(); got != "default-model" {
		t.Fatalf("the fallback model was %q", got)
	}
	// The instructions are the template's own, with the shared rule after them, and
	// never the main core's prompt, because a sub agent on its parent's prompt would
	// inherit permissions nobody meant to hand it.
	hint := provider.systemHint()
	if !strings.Contains(hint, "Do the thing.") {
		t.Fatalf("the instructions are %q", hint)
	}
	if !strings.Contains(hint, "Answer in the language of the task.") {
		t.Fatalf("the shared rule is missing from %q", hint)
	}
	// The reserved list is reported back so a caller can check its own configuration
	// rather than finding out from a sub agent that came back without a tool.
	reservedNames := builder.ReservedNames()
	if len(reservedNames) == 0 {
		t.Fatal("the builder reports no reserved names")
	}
	for index := 1; index < len(reservedNames); index++ {
		if reservedNames[index-1] >= reservedNames[index] {
			t.Fatalf("the reserved names are not in order: %v", reservedNames)
		}
	}
}

// The builder is not usable without the three things it needs, and saying so at
// construction is better than failing on the first task.
func TestTheBuilderSaysWhatItIsMissing(t *testing.T) {
	resolver, _ := stubOrchestration()
	cases := []struct {
		name   string
		config BuilderConfig
	}{
		{"no provider", BuilderConfig{MainID: "main", Tools: resolver}},
		{"no main core", BuilderConfig{Provider: &recordingProvider{}, Tools: resolver}},
		{"no way to resolve tools", BuilderConfig{Provider: &recordingProvider{}, MainID: "main"}},
	}
	for _, testCase := range cases {
		if _, err := NewCoreBuilder(testCase.config); err == nil {
			t.Fatalf("the builder was built with %s", testCase.name)
		}
	}
	// The defaults are filled in rather than left at zero, which would bound a sub
	// agent's loop to nothing.
	builder, err := NewCoreBuilder(BuilderConfig{
		Provider: &recordingProvider{}, MainID: "main", Tools: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	if builder.config.Defaults.MaxIterations <= 0 {
		t.Fatalf("the loop limit is %d", builder.config.Defaults.MaxIterations)
	}
	if builder.config.Defaults.SessionPrefix == "" {
		t.Fatal("no session prefix was filled in")
	}
	// A cancelled caller and a missing slot are both refused rather than built for.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := builder.Build(ctx, Template{Name: "t"}, &Slot{ID: "slot-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("building for a cancelled caller: %v", err)
	}
	if _, err := builder.Build(context.Background(), Template{Name: "t"}, nil); err == nil {
		t.Fatal("the builder worked with no slot")
	}
}

// nameSet turns a list of names into a set, because a list cannot be asked whether it
// holds a name and pretending otherwise is how a check quietly stops checking.
func nameSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

// stubOrchestration builds a resolver holding the ordinary tools and the orchestration
// tools, and the list of orchestration names that must never reach a sub agent.
func stubOrchestration() (ToolResolver, []string) {
	names := []string{
		"spawn_subagent", "start_subagent", "stop_subagent", "delete_subagent",
		"list_subagents", "read_subagent_result", "send_subagent_message",
		"read_subagent_messages",
	}
	available := map[string]core.Tool{
		"read": &namedTool{"read"}, "write": &namedTool{"write"},
		"grep": &namedTool{"grep"}, "glob": &namedTool{"glob"},
		"spawn_subagent": &namedTool{"spawn_subagent"}, "start_subagent": &namedTool{"start_subagent"},
		"stop_subagent": &namedTool{"stop_subagent"}, "delete_subagent": &namedTool{"delete_subagent"},
		"list_subagents":         &namedTool{"list_subagents"},
		"read_subagent_result":   &namedTool{"read_subagent_result"},
		"send_subagent_message":  &namedTool{"send_subagent_message"},
		"read_subagent_messages": &namedTool{"read_subagent_messages"},
	}
	return ResolverFunc(func(name string) (core.Tool, bool) {
		tool, ok := available[name]
		return tool, ok
	}), names
}

// namedTool is a tool that exists only to be offered.
type namedTool struct{ name string }

func (t *namedTool) Name() string        { return t.name }
func (t *namedTool) Description() string { return "A tool named " + t.name + ", which does nothing." }
func (t *namedTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *namedTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	return core.ToolResult{Output: json.RawMessage(`{}`)}, nil
}

// recordingProvider answers with a single piece of text and records what it was
// asked, so a test can see what a sub agent was actually offered.
type recordingProvider struct {
	mu        sync.Mutex
	names     []string
	lastModel string
	hint      string
	messages  []core.Message
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) ChatStream(ctx context.Context, req core.ChatRequest) (<-chan core.StreamChunk, error) {
	p.mu.Lock()
	p.names = p.names[:0]
	for _, spec := range req.Tools {
		p.names = append(p.names, spec.Name)
	}
	p.lastModel = req.Model
	p.hint = req.SystemHint
	p.messages = append([]core.Message(nil), req.Messages...)
	p.mu.Unlock()

	out := make(chan core.StreamChunk, 1)
	out <- core.StreamChunk{Type: "text", Text: "done"}
	close(out)
	return out, nil
}

// toolNames reports the tools the last request declared.
func (p *recordingProvider) toolNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.names...)
}

// model reports the model the last request named.
func (p *recordingProvider) model() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastModel
}

// systemHint reports the instructions the last request carried.
func (p *recordingProvider) systemHint() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hint
}

// lastRequest reports the messages of the last request and the text in them.
func (p *recordingProvider) lastRequest() ([]core.Message, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var text strings.Builder
	for _, message := range p.messages {
		for _, block := range message.Content {
			text.WriteString(block.Text)
		}
	}
	return append([]core.Message(nil), p.messages...), text.String()
}

// writeTemplateWith writes a template that asks for a given set of tools, optionally
// on a given model.
func writeTemplateWith(t *testing.T, root, name, model string, tools []string) string {
	t.Helper()
	var frontmatter strings.Builder
	fmt.Fprintf(&frontmatter, "---\nname: %s\ndescription: A sub agent for tests.\n", name)
	if model != "" {
		fmt.Fprintf(&frontmatter, "model: %s\n", model)
	}
	frontmatter.WriteString("tools:\n")
	for _, tool := range tools {
		fmt.Fprintf(&frontmatter, "  - %s\n", tool)
	}
	frontmatter.WriteString("---\nDo the thing.\n")
	return writeTemplate(t, root, name, frontmatter.String())
}

// newTestPoolWithBuilder is newTestPool with a builder that really builds agents, so
// the confinement rule can be checked against what reached the model.
func newTestPoolWithBuilder(t *testing.T, builder AgentBuilder, capacity, concurrent int) (*Pool, *Registry, *Broker) {
	t.Helper()
	dir := t.TempDir()
	templates := newTemplatePool(t, dir, "")
	// A template for every name these tests ask for, each asking for every tool, so
	// the test chooses what to forbid rather than the fixture deciding for it.
	everyTool := []string{
		"read", "write", "grep", "glob",
		"spawn_subagent", "start_subagent", "stop_subagent", "delete_subagent",
		"list_subagents", "read_subagent_result", "send_subagent_message",
		"read_subagent_messages", "no_such_tool",
	}
	// Each template asks for every tool, so what a sub agent ends up with is decided
	// by the builder refusing and not by the fixture being careful.
	for _, name := range []string{"greedy", "hopeful", "many", "plain", "reviewer"} {
		writeTemplateWith(t, dir, name, "", everyTool)
	}
	// This one names a model, so the test can tell a template's choice from a default.
	writeTemplateWith(t, dir, "chosen", "chosen-model", everyTool)
	if err := templates.Refresh(context.Background()); err != nil {
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
