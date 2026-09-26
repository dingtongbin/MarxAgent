// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A gate is the only thing between a model's decision and a tool, and it has two
// ways to stop a call: keep it and mark it refused, or drop it from the list.
// Both have to leave the conversation in a state a provider will accept, which
// is one result per call. A refusal that went unanswered would fail the whole
// turn on the next request, so these cases are about the answer, not about the
// gate.

func toolUseCall(id, name, input string) ContentBlock {
	return ContentBlock{
		Type:       ContentTypeToolUse,
		ToolCallID: id,
		ToolName:   name,
		Input:      json.RawMessage(input),
	}
}

func toolCallChunkOf(call ContentBlock) StreamChunk {
	return StreamChunk{Type: StreamTypeToolCall, ToolCall: &call}
}

func TestAGateThatDropsACallStillLeavesItAnswered(t *testing.T) {
	executed := 0
	tool := testTool{
		name: "bash", description: "run a command", parameters: json.RawMessage(`{"type":"object"}`),
		execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			executed++
			return ToolResult{Output: json.RawMessage(`{"output":"ran"}`)}, nil
		},
	}
	provider := &recordingProvider{streams: [][]StreamChunk{
		{toolCallChunkOf(toolUseCall("call-1", "bash", `{"command":"rm -rf /"}`))},
		{{Type: StreamTypeText, Text: "I will not do that."}},
	}}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	// The gate refuses by removing the call, which is the version of refusing that
	// cannot carry a reason.
	if err := agent.Hooks().Register(HookToolCallReceived, 0,
		func(ctx context.Context, data any) (any, error) {
			return []ContentBlock{}, nil
		}); err != nil {
		t.Fatal(err)
	}
	events, err := agent.Run(context.Background(), textInput("delete everything"))
	if err != nil {
		t.Fatal(err)
	}
	collected := collectRunEvents(t, events)
	if !containsEventType(collected, EventTypeDone) {
		t.Fatalf("events = %v", eventTypes(collected))
	}
	if executed != 0 {
		t.Fatal("a refused call reached the tool")
	}
	// The second request is the one that would have been rejected, so it is the
	// one that has to carry an answer.
	requests := provider.Requests()
	if len(requests) < 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	results := toolResultBlocks(t, requests[1])
	if len(results) != 1 {
		t.Fatalf("results = %d, want one per call", len(results))
	}
	if !results[0].IsError {
		t.Fatal("a refusal was not reported as an error")
	}
	if !strings.Contains(string(results[0].Output), "refused by policy") {
		t.Fatalf("output = %s", results[0].Output)
	}
	// A result the model cannot act on is a result that wastes a turn, so the
	// refusal has to say which tool it was about.
	if !strings.Contains(string(results[0].Output), "bash") {
		t.Fatalf("output = %s", results[0].Output)
	}
}

func TestAGateThatKeepsACallCanSayWhyInItsOwnWords(t *testing.T) {
	executed := 0
	tool := testTool{
		name: "bash", description: "run a command", parameters: json.RawMessage(`{"type":"object"}`),
		execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			executed++
			return ToolResult{Output: json.RawMessage(`{"output":"ran"}`)}, nil
		},
	}
	provider := &recordingProvider{streams: [][]StreamChunk{
		{toolCallChunkOf(toolUseCall("call-1", "bash", `{"command":"rm -rf /"}`))},
		{{Type: StreamTypeText, Text: "Understood."}},
	}}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	const reason = "a shell is not available in this mode"
	if err := agent.Hooks().Register(HookToolCallReceived, 0,
		func(ctx context.Context, data any) (any, error) {
			calls, ok := data.([]ContentBlock)
			if !ok {
				return nil, errWrongPayload
			}
			for index := range calls {
				calls[index].IsError = true
				calls[index].Output = json.RawMessage(`"` + reason + `"`)
			}
			return calls, nil
		}); err != nil {
		t.Fatal(err)
	}
	events, err := agent.Run(context.Background(), textInput("delete everything"))
	if err != nil {
		t.Fatal(err)
	}
	collected := collectRunEvents(t, events)
	if !containsEventType(collected, EventTypeDone) {
		t.Fatalf("events = %v", eventTypes(collected))
	}
	if executed != 0 {
		t.Fatal("a refused call reached the tool")
	}
	requests := provider.Requests()
	results := toolResultBlocks(t, requests[len(requests)-1])
	if len(results) != 1 {
		t.Fatalf("results = %d", len(results))
	}
	// The gate's own words are what reach the model, because a refusal it cannot
	// interpret teaches it nothing about what to do differently.
	var text string
	if err := json.Unmarshal(results[0].Output, &text); err != nil {
		t.Fatal(err)
	}
	if text != reason {
		t.Fatalf("output = %q, want the gate's reason %q", text, reason)
	}
	if !results[0].IsError {
		t.Fatal("a refusal was not reported as an error")
	}
}

func TestARefusedCallStillPassesThroughTheExecutionHooks(t *testing.T) {
	tool := testTool{
		name: "read", description: "read a file", parameters: json.RawMessage(`{"type":"object"}`),
		execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			t.Error("a refused call reached the tool")
			return ToolResult{}, nil
		},
	}
	provider := &recordingProvider{streams: [][]StreamChunk{
		{toolCallChunkOf(toolUseCall("call-1", "read", `{"path":"a"}`))},
		{{Type: StreamTypeText, Text: "Done."}},
	}}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	if err := agent.Hooks().Register(HookToolCallReceived, 0,
		func(ctx context.Context, data any) (any, error) {
			calls := data.([]ContentBlock)
			for index := range calls {
				calls[index].IsError = true
				calls[index].Output = json.RawMessage(`"refused"`)
			}
			return calls, nil
		}); err != nil {
		t.Fatal(err)
	}
	// An attempt that was stopped is exactly what an audit has to be able to show,
	// so the attempt is recorded even though nothing ran.
	var audited []string
	if err := agent.Hooks().Register(HookPostToolExec, 0,
		func(ctx context.Context, data any) (any, error) {
			results := data.([]ToolResult)
			for _, result := range results {
				audited = append(audited, result.ToolName+":"+result.Error)
			}
			return results, nil
		}); err != nil {
		t.Fatal(err)
	}
	events, err := agent.Run(context.Background(), textInput("read a"))
	if err != nil {
		t.Fatal(err)
	}
	collectRunEvents(t, events)
	if len(audited) != 1 || !strings.Contains(audited[0], "refused") {
		t.Fatalf("audited = %v", audited)
	}
}

func TestAGateMayRewriteACallWithoutLosingIt(t *testing.T) {
	var seen string
	tool := testTool{
		name: "read", description: "read a file", parameters: json.RawMessage(`{"type":"object"}`),
		execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			seen = string(params)
			return ToolResult{Output: json.RawMessage(`{"content":"a"}`)}, nil
		},
	}
	provider := &recordingProvider{streams: [][]StreamChunk{
		{toolCallChunkOf(toolUseCall("call-1", "read", `{"path":"../escape"}`))},
		{{Type: StreamTypeText, Text: "Done."}},
	}}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	// A gate that confines a path has to be able to correct the argument, so a
	// kept call is the gate's version rather than the model's.
	if err := agent.Hooks().Register(HookToolCallReceived, 0,
		func(ctx context.Context, data any) (any, error) {
			calls := data.([]ContentBlock)
			calls[0].Input = json.RawMessage(`{"path":"inside"}`)
			return calls, nil
		}); err != nil {
		t.Fatal(err)
	}
	events, err := agent.Run(context.Background(), textInput("read something"))
	if err != nil {
		t.Fatal(err)
	}
	collectRunEvents(t, events)
	if !strings.Contains(seen, "inside") {
		t.Fatalf("the tool saw %q, want the gate's corrected path", seen)
	}
}

func TestARefusalWithNoUsableReasonStillAnswersTheCall(t *testing.T) {
	// A gate that marks a call refused but supplies nothing to say has still made
	// a decision, and the provider still needs an answer, so the loop supplies the
	// reason rather than leaving the call unanswered.
	invocation := ToolInvocation{ToolCallID: "call-1", ToolName: "bash"}
	for _, output := range []json.RawMessage{nil, json.RawMessage(`""`), json.RawMessage(`{}`)} {
		result := refusedToolResult(invocation, ContentBlock{Output: output})
		if !result.IsError {
			t.Fatalf("output %s: not reported as an error", output)
		}
		if result.Error == "" || !strings.Contains(result.Error, "bash") {
			t.Fatalf("output %s: reason = %q", output, result.Error)
		}
		if !json.Valid(result.Output) {
			t.Fatalf("output %s: the answer is not a record: %s", output, result.Output)
		}
	}
}

func TestReconcileRefusedCallsKeepsOrderAndIdentity(t *testing.T) {
	original := []ContentBlock{
		toolUseCall("call-1", "read", `{"path":"a"}`),
		toolUseCall("call-2", "bash", `{"command":"ls"}`),
		toolUseCall("call-3", "write", `{"path":"b"}`),
	}
	// The middle call is kept with its input rewritten, the last is dropped, and
	// the first is kept as it was.
	accepted := []ContentBlock{
		original[0],
		func() ContentBlock {
			rewritten := original[1]
			rewritten.Input = json.RawMessage(`{"command":"ls -la"}`)
			return rewritten
		}(),
	}
	merged := reconcileRefusedCalls(original, accepted)
	if len(merged) != len(original) {
		t.Fatalf("merged = %d calls, want %d", len(merged), len(original))
	}
	for index, call := range merged {
		// The order and the identity are what let a provider match a result to the
		// call it belongs to, so neither may be disturbed by a gate.
		if call.ToolCallID != original[index].ToolCallID {
			t.Fatalf("call %d has id %q, want %q", index, call.ToolCallID, original[index].ToolCallID)
		}
	}
	if string(merged[0].Input) != `{"path":"a"}` {
		t.Fatalf("input = %s", merged[0].Input)
	}
	if !strings.Contains(string(merged[1].Input), "-la") {
		t.Fatalf("the gate's correction was lost: %s", merged[1].Input)
	}
	if !merged[2].IsError {
		t.Fatal("the dropped call was not refused")
	}
	if merged[2].ToolName != "write" {
		t.Fatalf("the refused call lost its name: %q", merged[2].ToolName)
	}
	// The original slice is the model's record of what it asked for, so putting a
	// refusal into it would rewrite history.
	if original[2].IsError {
		t.Fatal("the input was modified")
	}
}

func toolResultBlocks(t *testing.T, request ChatRequest) []ContentBlock {
	t.Helper()
	var results []ContentBlock
	for _, message := range request.Messages {
		if message.Role != RoleTool {
			continue
		}
		for _, block := range message.Content {
			if block.Type == ContentTypeToolResult {
				results = append(results, block)
			}
		}
	}
	return results
}

var errWrongPayload = &wrongPayloadError{}

type wrongPayloadError struct{}

func (*wrongPayloadError) Error() string { return "the hook was handed the wrong payload" }
