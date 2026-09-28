// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/assembly/mode"
	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// captureProvider is a scripted provider that remembers what it was asked, so a
// test can assert on the request the assembly built rather than only on the
// answer the provider gave.
type captureProvider struct {
	mu       sync.Mutex
	requests []core.ChatRequest
	scripts  [][]core.StreamChunk
}

func (p *captureProvider) Name() string { return "capture" }

func (p *captureProvider) ChatStream(_ context.Context, request core.ChatRequest) (<-chan core.StreamChunk, error) {
	p.mu.Lock()
	index := len(p.requests)
	p.requests = append(p.requests, request)
	script := p.scripts
	p.mu.Unlock()
	if index >= len(script) {
		closed := make(chan core.StreamChunk)
		close(closed)
		return closed, nil
	}
	out := make(chan core.StreamChunk, len(script[index]))
	for _, chunk := range script[index] {
		out <- chunk
	}
	close(out)
	return out, nil
}

func (p *captureProvider) Requests() []core.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]core.ChatRequest(nil), p.requests...)
}

// A coding conversation is assembled as one: the model is told how to work as a
// coding agent, it is handed the project's tools, and its loop runs long enough
// to actually make a change.
func TestACodingSessionAssemblesTheCodingMode(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &captureProvider{scripts: [][]core.StreamChunk{
		textChunks("I read before I edit"),
	}}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{Project: project})
	if err != nil {
		t.Fatal(err)
	}
	if created.Mode != ModeCoding {
		t.Fatalf("mode = %q, want coding", created.Mode)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "fix the bug"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to settle", func() bool {
		info, err := sessions.snapshotSession(created.ID)
		return err == nil && info.State != StateRunning
	})
	requests := provider.Requests()
	if len(requests) != 1 {
		t.Fatalf("provider calls = %d", len(requests))
	}
	hint := requests[0].SystemHint
	if !strings.Contains(hint, "coding agent") {
		t.Fatalf("the coding prompt is not standing in front of the turn: %q", hint)
	}
	if !strings.Contains(hint, "Project directory: ") {
		t.Fatalf("the project snapshot is not part of the standing context: %q", hint)
	}
	// A coding conversation assembles the parts the mode holds. The project
	// here has no go.mod, so the language server contributes nothing and the
	// tool set is the six working parts, the two skill tools, the wiki's two
	// and the sub agent orchestration's eight.
	if len(requests[0].Tools) != 18 {
		t.Fatalf("tools = %d, want the assembled set", len(requests[0].Tools))
	}
	// The assembled set is presented in the pool's stable alphabetical order,
	// which is what keeps the request byte for byte the same between turns.
	names := make([]string, 0, len(requests[0].Tools))
	for _, spec := range requests[0].Tools {
		names = append(names, spec.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("the tool set is not in the stable order: %v", names)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		seen[name] = struct{}{}
	}
	for _, want := range []string{
		"read", "write", "edit", "glob", "grep", "bash",
		"discover_skill", "activate_skill", "wiki_search", "wiki_read",
		"spawn_subagent", "start_subagent", "read_subagent_result",
	} {
		if _, held := seen[want]; !held {
			t.Fatalf("%q is not in the assembled set: %v", want, names)
		}
	}
	// The language server is not part of it: the project has no go.mod, and a
	// project with no server is a conversation with no lsp tools.
	for _, absent := range []string{"lsp_hover", "lsp_rename"} {
		if _, held := seen[absent]; held {
			t.Fatalf("%q came from a project with no language server", absent)
		}
	}
}

// A chat conversation is assembled as one: the model is told how to answer
// without tools, the request carries none, and a call the model invents anyway
// is answered by the chat mode's gate rather than reaching the machine.
func TestAChatSessionAssemblesTheChatMode(t *testing.T) {
	provider := &captureProvider{scripts: [][]core.StreamChunk{
		{{
			Type: core.StreamTypeToolCall,
			ToolCall: &core.ContentBlock{
				Type:       core.ContentTypeToolUse,
				ToolCallID: "call-1",
				ToolName:   "read",
				Input:      json.RawMessage(`{"path":"C:/anything"}`),
			},
		}, {Type: core.StreamTypeDone}},
		textChunks("I have no tools"),
	}}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Mode != ModeChat {
		t.Fatalf("mode = %q, want chat", created.Mode)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "say something"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to settle", func() bool {
		info, err := sessions.snapshotSession(created.ID)
		return err == nil && info.State != StateRunning
	})
	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider calls = %d, want the call and the continuation", len(requests))
	}
	if len(requests[0].Tools) != 0 {
		t.Fatalf("tools = %d, want a chat turn to carry none", len(requests[0].Tools))
	}
	hint := requests[0].SystemHint
	if !strings.Contains(hint, "conversational assistant") || !strings.Contains(hint, "no tools") {
		t.Fatalf("the chat prompt is not standing in front of the turn: %q", hint)
	}
	// The invented call was refused by the gate, in the envelope the model can
	// read, and the turn went on to its answer.
	refused := false
	for _, message := range requests[1].Messages {
		for _, block := range message.Content {
			if block.Type != core.ContentTypeToolResult || !block.IsError {
				continue
			}
			var quoted string
			if err := json.Unmarshal(block.Output, &quoted); err != nil {
				continue
			}
			if strings.Contains(quoted, "no tools") {
				refused = true
			}
		}
	}
	if !refused {
		t.Fatalf("the invented call was not refused with the chat rule: %#v", requests[1].Messages)
	}
}

// A turn sent under plan runs on read and search only: the request carries
// the three read-only tools, a call to change the project is refused with the
// plan rule, and the level the turn ran at is what the conversation reports.
func TestAPlanTurnIsReadAndSearchOnly(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &captureProvider{scripts: [][]core.StreamChunk{
		{{
			Type: core.StreamTypeToolCall,
			ToolCall: &core.ContentBlock{
				Type:       core.ContentTypeToolUse,
				ToolCallID: "call-1",
				ToolName:   "write",
				Input:      json.RawMessage(`{"path":"` + filepath.ToSlash(filepath.Join(project, "x.txt")) + `","content":"no"}`),
			},
		}, {Type: core.StreamTypeDone}},
		textChunks("planned it"),
	}}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{Project: project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{
		Text:      "plan the change",
		Execution: "plan",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to settle", func() bool {
		info, err := sessions.snapshotSession(created.ID)
		return err == nil && info.State != StateRunning
	})
	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider calls = %d, want the call and the continuation", len(requests))
	}
	tools := requests[0].Tools
	// Plan reads and searches: the three read-only built-ins plus the skills,
	// the wiki and the sub agent orchestration it may look at, and nothing
	// that can change the project.
	if len(tools) != 15 {
		t.Fatalf("tools = %d, want plan's read-only set", len(tools))
	}
	for _, spec := range tools {
		if spec.Name == "write" || spec.Name == "edit" || spec.Name == "bash" {
			t.Fatalf("plan carries %q, which can change the project", spec.Name)
		}
	}
	refused := false
	for _, message := range requests[1].Messages {
		for _, block := range message.Content {
			if block.Type != core.ContentTypeToolResult || !block.IsError {
				continue
			}
			var quoted string
			if err := json.Unmarshal(block.Output, &quoted); err != nil {
				continue
			}
			if strings.Contains(quoted, "not available in this mode") {
				refused = true
			}
		}
	}
	if !refused {
		t.Fatalf("the write call was not refused by the plan rule: %#v", requests[1].Messages)
	}
	info, err := sessions.snapshotSession(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Execution != "plan" {
		t.Fatalf("execution = %q, want plan after the turn that ran under it", info.Execution)
	}
}

// A turn sent under full access runs without the gate: a command the build
// level refuses by name is executed for real, and the shell is the one that
// answers rather than the one the refusal would have come from.
func TestAFullAccessTurnRunsUnrefused(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &captureProvider{scripts: [][]core.StreamChunk{
		{{
			Type: core.StreamTypeToolCall,
			ToolCall: &core.ContentBlock{
				Type:       core.ContentTypeToolUse,
				ToolCallID: "call-1",
				ToolName:   "bash",
				Input:      json.RawMessage(`{"command":"echo ran-anyway"}`),
			},
		}, {Type: core.StreamTypeDone}},
		textChunks("it ran"),
	}}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{Project: project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{
		Text:      "run it",
		Execution: "full",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to settle", func() bool {
		info, err := sessions.snapshotSession(created.ID)
		return err == nil && info.State != StateRunning
	})
	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider calls = %d, want the call and the continuation", len(requests))
	}
	// Full access runs the same assembled set ungated: the built-ins, the
	// skills, the wiki and the orchestration, with no gate behind them.
	if len(requests[0].Tools) != 18 {
		t.Fatalf("tools = %d, want the assembled set ungated", len(requests[0].Tools))
	}
	// The command actually ran and its output came back, rather than a refusal
	// naming a rule. The envelope is still the untrusted one, so the refusal a
	// gated level would have written is what the assertion looks for the
	// absence of.
	ran := false
	for _, message := range requests[1].Messages {
		for _, block := range message.Content {
			if block.Type != core.ContentTypeToolResult || block.IsError {
				continue
			}
			if strings.Contains(string(block.Output), "ran-anyway") {
				ran = true
			}
		}
	}
	if !ran {
		t.Fatalf("the command did not run under full access: %#v", requests[1].Messages)
	}
	// And an unknown level is refused before the turn starts, because a turn
	// that ran at a level nobody asked for is a reach nobody can account for.
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{
		Text:      "again",
		Execution: "sudo",
	}); err == nil {
		t.Fatal("an unknown execution mode was accepted")
	}
}

// The loop of a coding turn runs past a conversational turn's budget, because a
// change is a search before it is an edit.
func TestACodingTurnRunsLongerThanAChatTurn(t *testing.T) {
	if mode.CodingMaxIterations <= core.DefaultMaxIterations {
		t.Fatalf("the coding budget %d does not exceed the chat budget %d",
			mode.CodingMaxIterations, core.DefaultMaxIterations)
	}
}

// snapshotSession reads a session's info the way List does, so a test can wait
// for a state without reaching into private fields.
func (r *Runtime) snapshotSession(sessionID string) (SessionInfo, error) {
	session, err := r.lookup(sessionID)
	if err != nil {
		return SessionInfo{}, err
	}
	return r.infoSnapshot(session), nil
}

// A coding conversation's gates answer a refused call with the rule that
// refused it, so the model can read the reason and go on with its turn.
func TestACodingSessionRefusesADangerousCommandAndCarriesOn(t *testing.T) {
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := &captureProvider{scripts: [][]core.StreamChunk{
		{{
			Type: core.StreamTypeToolCall,
			ToolCall: &core.ContentBlock{
				Type:       core.ContentTypeToolUse,
				ToolCallID: "call-1",
				ToolName:   "bash",
				Input:      json.RawMessage(`{"command":"rm -rf /"}`),
			},
		}, {Type: core.StreamTypeDone}},
		textChunks("I refused that"),
	}}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{Project: project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "clean up"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the turn to settle", func() bool {
		info, err := sessions.snapshotSession(created.ID)
		return err == nil && info.State != StateRunning
	})
	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider calls = %d, want the call and the continuation", len(requests))
	}
	refused := false
	for _, message := range requests[1].Messages {
		for _, block := range message.Content {
			if block.Type != core.ContentTypeToolResult || !block.IsError {
				continue
			}
			// The refusal travels wrapped in the untrusted envelope, which is a
			// JSON string whose body carries the rule that decided.
			var quoted string
			if err := json.Unmarshal(block.Output, &quoted); err != nil {
				continue
			}
			if strings.Contains(quoted, "rm") {
				refused = true
			}
		}
	}
	if !refused {
		t.Fatalf("the dangerous call was not refused with its reason: %#v", requests[1].Messages)
	}
}

// After a restart the startup has already opened every project's scope and
// registered it by its data root. A conversation created for one of those
// projects has to reuse that scope rather than open the ledger again and be
// refused by the lock this process itself is holding.
func TestAScopeTheStartupOpenedIsReusedForTheNextConversation(t *testing.T) {
	sessions, _ := newTestRuntime(t)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Create(CreateRequest{Project: project}); err != nil {
		t.Fatal(err)
	}
	kept := sessions.scopes[projectScopeKey(project)]
	if kept == nil {
		t.Fatal("the conversation's scope was not registered")
	}
	// The state of a runtime that has just come back: one scope per project,
	// registered by the data root, and nothing registered by the project root
	// that a Create names.
	sessions.mu.Lock()
	sessions.scopes = map[string]*storage.Scope{projectScopeKey(project): kept}
	sessions.mu.Unlock()
	second, err := sessions.Create(CreateRequest{Project: project})
	if err != nil {
		t.Fatalf("the scope the startup opened was not reused: %v", err)
	}
	if second.Mode != ModeCoding {
		t.Fatalf("mode = %q, want coding", second.Mode)
	}
}

// A conversation stored by an earlier build under the mode name "work" is the
// same conversation: it comes back reported as coding, with its data untouched.
func TestAStoredWorkSessionIsReportedAsCoding(t *testing.T) {
	sessions, _ := newTestRuntime(t)
	appData := sessions.options.AppDataScope
	directory, err := appData.EnsureSessionDir("session-restored")
	if err != nil {
		t.Fatal(err)
	}
	summary := storage.SessionSummary{
		SessionID: "session-restored",
		Mode:      ModeWork,
		Project:   filepath.Join(t.TempDir(), "project"),
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, storage.SummaryFileName), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, stream := range []string{"session", "events"} {
		path := filepath.Join(directory, stream+".jsonl")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	summaries, err := appData.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("listed = %#v", summaries)
	}
	state, err := sessions.loadSession(appData, summaries[0])
	if err != nil {
		t.Fatal(err)
	}
	// The load opened the journals and the buffer, and a file left open is a
	// temporary directory that cannot be cleaned up on this platform.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = state.buffer.Close(ctx)
		for _, journal := range state.journals {
			_ = journal.Close()
		}
	}()
	if state.info.Mode != ModeCoding {
		t.Fatalf("restored mode = %q, want coding", state.info.Mode)
	}
}
