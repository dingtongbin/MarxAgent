// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/security"
)

// newTestProject builds a project directory a test can point the coding mode at.
// It is inside a temporary directory, so the test neither reads nor writes the
// machine it runs on.
func newTestProject(t *testing.T) string {
	t.Helper()
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	return project
}

func writeProjectFile(t *testing.T, project, name, content string) {
	t.Helper()
	path := filepath.Join(project, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the directory of %s: %v", name, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func callFor(name, input string) []core.ContentBlock {
	return []core.ContentBlock{{
		Type:       core.ContentTypeToolUse,
		ToolCallID: "call-1",
		ToolName:   name,
		Input:      json.RawMessage(input),
	}}
}

func TestNewCodingBuildsTheAssemblyAndRefusesAnEmptyOne(t *testing.T) {
	project := newTestProject(t)
	assembly, err := NewCoding(project, Parts{})
	if err != nil {
		t.Fatalf("NewCoding() error = %v", err)
	}
	if assembly.Root() == "" || assembly.Root() != filepath.Clean(project) && !strings.EqualFold(assembly.Root(), filepath.Clean(project)) {
		t.Fatalf("root = %q, want the project directory", assembly.Root())
	}
	if _, err := NewCoding("", Parts{}); err == nil {
		t.Fatal("an empty root was accepted")
	}
	if _, err := NewCoding(filepath.Join(t.TempDir(), "absent"), Parts{}); err == nil {
		t.Fatal("a directory that does not exist was accepted")
	}
	notDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCoding(notDirectory, Parts{}); err == nil {
		t.Fatal("a file was accepted as a project")
	}
}

// A nil assembly answers as the empty one rather than as a fault: every hook and
// every getter has to survive being asked on a value the caller never built,
// because a nil assembly is a conversation whose turn started before its
// assembly did.
func TestNilAssemblyIsSafeToAsk(t *testing.T) {
	var assembly *Coding
	if assembly.Root() != "" {
		t.Fatal("nil root is not empty")
	}
	if _, err := assembly.Tools(context.Background(), ExecBuild); err == nil {
		t.Fatal("nil assembly built tools")
	}
	if prompt := assembly.SystemPrompt(); !strings.Contains(prompt, CodingPrompt) {
		t.Fatalf("nil prompt = %q", prompt)
	}
	if err := assembly.RegisterHooks(nil, ExecBuild); err == nil {
		t.Fatal("nil assembly registered hooks")
	}
}

func TestCodingToolsAreTheSixWorkingParts(t *testing.T) {
	assembly, err := NewCoding(newTestProject(t), Parts{})
	if err != nil {
		t.Fatal(err)
	}
	toolSet, err := assembly.Tools(context.Background(), ExecBuild)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(toolSet))
	for _, tool := range toolSet {
		names = append(names, tool.Name())
	}
	want := []string{"read", "write", "edit", "glob", "grep", "bash"}
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	seen := make(map[string]struct{}, len(want))
	for _, name := range names {
		seen[name] = struct{}{}
	}
	for _, name := range want {
		if _, exists := seen[name]; !exists {
			t.Fatalf("tool %q is missing from %v", name, names)
		}
	}
}

// The same assembly hands out the same shell tool twice rather than opening a
// second one, because a shell per call to Tools would be a shell per turn. The
// file and search tools are cheap stateless values, so only the shell is
// identity-checked.
func TestCodingShellToolIsBuiltOnce(t *testing.T) {
	assembly, err := NewCoding(newTestProject(t), Parts{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := assembly.Tools(context.Background(), ExecBuild)
	if err != nil {
		t.Fatal(err)
	}
	second, err := assembly.Tools(context.Background(), ExecBuild)
	if err != nil {
		t.Fatal(err)
	}
	if first[len(first)-1] != second[len(second)-1] {
		t.Fatal("the shell tool was rebuilt instead of reused")
	}
	for index := range first {
		if first[index].Name() != second[index].Name() {
			t.Fatalf("tool %d changed name between builds: %q vs %q", index, first[index].Name(), second[index].Name())
		}
	}
}

func TestCodingGateDecides(t *testing.T) {
	project := newTestProject(t)
	assembly, err := NewCoding(project, Parts{})
	if err != nil {
		t.Fatal(err)
	}
	// Building the level hands the test the policy that level's gate runs.
	if _, err := assembly.Tools(context.Background(), ExecBuild); err != nil {
		t.Fatal(err)
	}
	assembly.kitMu.Lock()
	policy := assembly.kits[ExecBuild].policy
	assembly.kitMu.Unlock()
	inside := filepath.Join(project, "main.go")
	tests := []struct {
		name     string
		tool     string
		input    string
		wantDeny string
	}{
		{
			name: "a read inside the project runs",
			tool: "read",
			input: `{"path":"` + filepath.ToSlash(inside) + `"}`,
		},
		{
			name:     "a write outside the project is refused",
			tool:     "write",
			input:    `{"path":"D:/elsewhere/x.txt","content":"no"}`,
			wantDeny: "outside the workspace",
		},
		{
			name:     "a tool the mode did not assemble is refused",
			tool:     "web_search",
			input:    `{}`,
			wantDeny: "not available in this mode",
		},
		{
			name:     "a build command runs",
			tool:     "bash",
			input:    `{"command":"go build ./..."}`,
		},
		{
			name:     "a dangerous command is refused with its rule",
			tool:     "bash",
			input:    `{"command":"rm -rf /"}`,
			wantDeny: "workspace.dangerous",
		},
		{
			name:     "a shell inside the shell is refused with its rule",
			tool:     "bash",
			input:    `{"command":"powershell -Command Get-Content x"}`,
			wantDeny: "workspace.dangerous",
		},
		{
			name:     "an unknown command is refused with its rule",
			tool:     "bash",
			input:    `{"command":"curl http://example.invalid"}`,
			wantDeny: "workspace.dangerous",
		},
		{
			name:     "an off-list command is refused by the list",
			tool:     "bash",
			input:    `{"command":"banana peel"}`,
			wantDeny: "not on the command list",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := policy.Evaluate(security.Call{
				ToolName: test.tool,
				Path:     pathOf(test.input),
				Command:  commandOf(test.input),
			})
			if test.wantDeny == "" {
				if !decision.Allowed {
					t.Fatalf("decision = %#v, want allowed", decision)
				}
				return
			}
			// A call the gate cannot grant is one the hook answers with a
			// refusal: either the policy refused it outright, or it needs a
			// human and this build has no approval queue to ask.
			if decision.Allowed && !decision.RequiresApproval {
				t.Fatalf("decision = %#v, want refused", decision)
			}
			if !strings.Contains(decision.Rule, test.wantDeny) && !strings.Contains(decision.Reason, test.wantDeny) {
				t.Fatalf("refusal = %#v, want it to name %q", decision, test.wantDeny)
			}
		})
	}
}

// pathOf lifts the path out of a JSON object the way the gate itself does, so a
// test asserts on what the gate sees rather than on a rendering of it.
func pathOf(input string) string {
	var fields struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		return ""
	}
	return fields.Path
}

// commandOf lifts the command the same way.
func commandOf(input string) string {
	var fields struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		return ""
	}
	return fields.Command
}

// A turn run on the assembled agent shows the whole assembly working: the model
// gets the coding prompt with the project context in it, the tools it asked for
// arrive, and a refused call is answered with the refusal rather than dropped.
type captureProvider struct {
	mu       chan<- struct{}
	requests []core.ChatRequest
	script   [][]core.StreamChunk
}

func (p *captureProvider) Name() string { return "capture" }

func (p *captureProvider) ChatStream(_ context.Context, request core.ChatRequest) (<-chan core.StreamChunk, error) {
	p.requests = append(p.requests, request)
	stream := make(chan core.StreamChunk, 4)
	chunks := []core.StreamChunk{}
	if len(p.requests) == 1 {
		chunks = []core.StreamChunk{{
			Type: core.StreamTypeToolCall,
			ToolCall: &core.ContentBlock{
				Type:       core.ContentTypeToolUse,
				ToolCallID: "call-danger",
				ToolName:   "bash",
				Input:      json.RawMessage(`{"command":"rm -rf ."}`),
			},
		}}
	} else {
		chunks = []core.StreamChunk{{Type: core.StreamTypeText, Text: "refused it"}}
	}
	chunks = append(chunks, core.StreamChunk{Type: core.StreamTypeDone})
	for _, chunk := range chunks {
		stream <- chunk
	}
	close(stream)
	return stream, nil
}

func TestCodingTurnAssemblesPromptToolsAndGate(t *testing.T) {
	project := newTestProject(t)
	assembly, err := NewCoding(project, Parts{})
	if err != nil {
		t.Fatal(err)
	}
	toolSet, err := assembly.Tools(context.Background(), ExecBuild)
	if err != nil {
		t.Fatal(err)
	}
	provider := &captureProvider{}
	agent, err := core.NewAgent(provider, core.Config{
		AgentID:       "test",
		SessionID:     "session",
		Model:         "test-model",
		SystemHint:    assembly.SystemPrompt(),
		MaxIterations: CodingMaxIterations,
		Tools:         toolSet,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.RegisterHooks(agent, ExecBuild); err != nil {
		t.Fatal(err)
	}
	events, err := agent.Run(context.Background(), core.Input{Type: core.InputTypeText, Content: "clean the build"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider calls = %d, want the call and the retry after the refusal", len(provider.requests))
	}
	first := provider.requests[0]
	if !strings.Contains(first.SystemHint, "coding agent") ||
		!strings.Contains(first.SystemHint, "untrusted data") ||
		!strings.Contains(first.SystemHint, "Project directory:") {
		t.Fatalf("the coding prompt is not standing in front of the turn: %q", first.SystemHint)
	}
	if len(first.Tools) != 6 {
		t.Fatalf("tools = %d, want 6", len(first.Tools))
	}
	// The injected context arrived as its own system message, which is the part
	// the formats hoist into the system prompt.
	sawContext := false
	for _, message := range first.Messages {
		if message.Role == core.RoleSystem && message.ID == "project-context" {
			sawContext = true
		}
	}
	if !sawContext {
		t.Fatalf("the project context was not injected: %#v", first.Messages)
	}
	// The refused call came back as a result the model can read, carrying the
	// rule that decided rather than a bare error.
	refusals := 0
	for _, message := range agent.State().Messages() {
		for _, block := range message.Content {
			if block.Type == core.ContentTypeToolResult && block.IsError {
				refusals++
				text, _ := strconvUnquote(block.Output)
				if !strings.Contains(text, "rm") && !strings.Contains(text, "human") {
					t.Fatalf("refusal = %q, want it to say why", block.Output)
				}
			}
		}
	}
	if refusals != 1 {
		t.Fatalf("refusal count = %d, want one for the dangerous call", refusals)
	}
}

// strconvUnquote reads a quoted JSON string body, tolerating an object that is
// not a bare string, which is what a wrapped result is.
func strconvUnquote(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			return value, true
		}
	}
	return string(raw), false
}
