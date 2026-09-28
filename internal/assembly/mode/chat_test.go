// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/security"
)

// A chat assembly answers every call with a refusal, whichever tool it is
// asked about and whatever the call carries: the conversation was opened for
// words, and the gate is what keeps it that way.
func TestChatGateRefusesEveryCall(t *testing.T) {
	assembly := NewChat()
	tests := []struct {
		name  string
		tool  string
		input string
	}{
		{name: "a file read is refused", tool: "read", input: `{"path":"C:/anything"}`},
		{name: "a file write is refused", tool: "write", input: `{"path":"C:/anything","content":"x"}`},
		{name: "a command is refused", tool: "bash", input: `{"command":"go version"}`},
		{name: "a search is refused", tool: "grep", input: `{"pattern":"x"}`},
		{name: "an unknown tool is refused", tool: "web_search", input: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := assembly.policy.Evaluate(gateCallFor(test.tool, test.input))
			if decision.Allowed || decision.RequiresApproval {
				t.Fatalf("decision = %#v, want refused", decision)
			}
			if !strings.Contains(decision.Reason, test.tool) {
				t.Fatalf("refusal = %q, want it to name the tool", decision.Reason)
			}
		})
	}
}

// gateCallFor builds a policy call the way a call arrives at the gate, so the
// test asserts on what the gate sees rather than on a rendering of it. The
// coding tests already have a callFor under another shape; this one lifts the
// path and the command the way the policy reads them.
func gateCallFor(name, input string) security.Call {
	call := security.Call{ToolName: name}
	var fields struct {
		Path    string `json:"path"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(input), &fields); err == nil {
		call.Path = fields.Path
		call.Command = fields.Command
	}
	return call
}

// A chat conversation carries no tools at all, and its prompt says so in the
// places that matter: what the mode is, what it does not have, and the
// language rule every mode's prompt ends with.
func TestChatAssemblyHasNoToolsAndSaysSo(t *testing.T) {
	assembly := NewChat()
	toolSet, err := assembly.Tools()
	if err != nil {
		t.Fatalf("Tools() error = %v", err)
	}
	if len(toolSet) != 0 {
		t.Fatalf("tools = %d, want none", len(toolSet))
	}
	prompt := assembly.SystemPrompt()
	if !strings.Contains(prompt, "conversational assistant") {
		t.Fatalf("the prompt does not say what the mode is: %q", prompt)
	}
	if !strings.Contains(prompt, "no tools") {
		t.Fatalf("the prompt does not say what the mode does not have: %q", prompt)
	}
	if !strings.Contains(prompt, "same natural language") {
		t.Fatalf("the language rule is missing from the prompt: %q", prompt)
	}
}

// A turn run on the assembled agent goes out without tools and comes back
// through the gate: a model that calls a tool it was never given gets the
// refusal the gate writes, and the turn carries on to its real answer.
func TestChatTurnRunsToollessAndBehindTheGate(t *testing.T) {
	assembly := NewChat()
	provider := &chatCaptureProvider{}
	agent, err := core.NewAgent(provider, core.Config{
		AgentID:       "test",
		SessionID:     "session",
		Model:         "test-model",
		SystemHint:    assembly.SystemPrompt(),
		MaxIterations: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembly.RegisterHooks(agent); err != nil {
		t.Fatal(err)
	}
	events, err := agent.Run(context.Background(), core.Input{Type: core.InputTypeText, Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider calls = %d, want the call and the retry after the refusal", len(provider.requests))
	}
	first := provider.requests[0]
	if len(first.Tools) != 0 {
		t.Fatalf("tools = %d, want the request to carry none", len(first.Tools))
	}
	if first.SystemHint != ChatPrompt {
		t.Fatal("the turn was not standing in front of the chat prompt")
	}
	refusals := 0
	for _, message := range agent.State().Messages() {
		for _, block := range message.Content {
			if block.Type == core.ContentTypeToolResult && block.IsError {
				refusals++
			}
		}
	}
	if refusals != 1 {
		t.Fatalf("refusal count = %d, want one for the call the gate refused", refusals)
	}
}

// chatCaptureProvider answers the first call with a tool call the model was
// never given a tool for, and the second with the answer it owed.
type chatCaptureProvider struct {
	requests []core.ChatRequest
}

func (p *chatCaptureProvider) Name() string { return "capture" }

func (p *chatCaptureProvider) ChatStream(_ context.Context, request core.ChatRequest) (<-chan core.StreamChunk, error) {
	p.requests = append(p.requests, request)
	stream := make(chan core.StreamChunk, 4)
	chunks := []core.StreamChunk{{
		Type: core.StreamTypeToolCall,
		ToolCall: &core.ContentBlock{
			Type:       core.ContentTypeToolUse,
			ToolCallID: "call-1",
			ToolName:   "read",
			Input:      json.RawMessage(`{"path":"C:/anything"}`),
		},
	}}
	if len(p.requests) > 1 {
		chunks = []core.StreamChunk{{Type: core.StreamTypeText, Text: "answered"}}
	}
	chunks = append(chunks, core.StreamChunk{Type: core.StreamTypeDone})
	for _, chunk := range chunks {
		stream <- chunk
	}
	close(stream)
	return stream, nil
}
