// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func responsesAdapterFor(t *testing.T) *openAIResponsesAdapter {
	t.Helper()
	adapter, err := NewAdapter(FormatOpenAIResponses)
	if err != nil {
		t.Fatal(err)
	}
	concrete, ok := adapter.(*openAIResponsesAdapter)
	if !ok {
		t.Fatalf("adapter = %T", adapter)
	}
	return concrete
}

func buildResponses(t *testing.T, request *core.ChatRequest) *responsesRequest {
	t.Helper()
	payload, err := responsesAdapterFor(t).BuildRequest(request)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	converted, ok := payload.(*responsesRequest)
	if !ok {
		t.Fatalf("payload = %T", payload)
	}
	return converted
}

func TestResponsesBuildRequestTable(t *testing.T) {
	base := func() *core.ChatRequest {
		return &core.ChatRequest{
			Model:    "gpt-4.1",
			Messages: []core.Message{textMessage(core.RoleUser, "1", "hello")},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*core.ChatRequest)
		check   func(*testing.T, *responsesRequest)
		wantErr string
	}{
		{
			name: "user text becomes an input item",
			check: func(t *testing.T, payload *responsesRequest) {
				if len(payload.Input) != 1 || payload.Input[0].Type != "message" || payload.Input[0].Role != "user" {
					t.Fatalf("input = %#v", payload.Input)
				}
				content := payload.Input[0].Content
				if len(content) != 1 || content[0].Type != "input_text" || content[0].Text != "hello" {
					t.Fatalf("content = %#v", content)
				}
				if !payload.Stream || !payload.Store {
					t.Fatalf("stream = %v store = %v", payload.Stream, payload.Store)
				}
			},
		},
		{
			name: "instructions carry the system prompt",
			mutate: func(request *core.ChatRequest) {
				request.SystemHint = "be terse"
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if payload.Instructions != "be terse" {
					t.Fatalf("instructions = %q", payload.Instructions)
				}
			},
		},
		{
			name: "hoisted system messages merge with the instructions",
			mutate: func(request *core.ChatRequest) {
				request.SystemHint = "be terse"
				request.Messages = append([]core.Message{textMessage(core.RoleSystem, "s", "history")}, request.Messages...)
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if payload.Instructions != "be terse\n\nhistory" {
					t.Fatalf("instructions = %q", payload.Instructions)
				}
			},
		},
		{
			name: "max output tokens uses the responses field name",
			mutate: func(request *core.ChatRequest) {
				request.MaxTokens = 900
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if payload.MaxOutputTokens != 900 {
					t.Fatalf("max output tokens = %d", payload.MaxOutputTokens)
				}
			},
		},
		{
			name: "temperature is optional",
			mutate: func(request *core.ChatRequest) {
				request.Temperature = 0.3
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if payload.Temperature == nil || *payload.Temperature != 0.3 {
					t.Fatalf("temperature = %v", payload.Temperature)
				}
			},
		},
		{
			name: "tools are flat function declarations",
			mutate: func(request *core.ChatRequest) {
				request.Tools = []core.ToolSpec{toolSpec("read")}
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if len(payload.Tools) != 1 || payload.Tools[0].Type != "function" || payload.Tools[0].Name != "read" {
					t.Fatalf("tools = %#v", payload.Tools)
				}
				if string(payload.Tools[0].Parameters) != `{"type":"object"}` {
					t.Fatalf("parameters = %s", payload.Tools[0].Parameters)
				}
			},
		},
		{
			name: "a tool without a schema gets an empty object schema",
			mutate: func(request *core.ChatRequest) {
				request.Tools = []core.ToolSpec{{Name: "ping", Description: "Ping."}}
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if string(payload.Tools[0].Parameters) != `{"type":"object","properties":{}}` {
					t.Fatalf("parameters = %s", payload.Tools[0].Parameters)
				}
			},
		},
		{
			name: "images become data url input parts",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{imageBlock("aGk=", "png")}
			},
			check: func(t *testing.T, payload *responsesRequest) {
				content := payload.Input[0].Content
				if len(content) != 1 || content[0].Type != "input_image" {
					t.Fatalf("content = %#v", content)
				}
				if content[0].ImageURL != "data:image/png;base64,aGk=" {
					t.Fatalf("image = %q", content[0].ImageURL)
				}
			},
		},
		{
			name: "an unsupported image type is rejected",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{imageBlock("aGk=", "image/tiff")}
			},
			wantErr: "unsupported image media type",
		},
		{
			name: "text and image parts keep their order",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeText, Text: "before"},
					imageBlock("aGk=", "png"),
					{Type: core.ContentTypeText, Text: "after"},
				}
			},
			check: func(t *testing.T, payload *responsesRequest) {
				content := payload.Input[0].Content
				if len(content) != 3 || content[0].Text != "before" || content[2].Text != "after" {
					t.Fatalf("content = %#v", content)
				}
			},
		},
		{
			name: "an empty user message is rejected",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeThinking, ThinkingSig: "sig"},
				}
			},
			wantErr: "cannot be sent in a user message",
		},
		{
			name: "assistant text becomes an output message",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleAssistant,
					Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "answer"}},
				})
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if len(payload.Input) != 2 {
					t.Fatalf("input = %#v", payload.Input)
				}
				assistant := payload.Input[1]
				if assistant.Type != "message" || assistant.Role != "assistant" {
					t.Fatalf("assistant = %#v", assistant)
				}
				if assistant.Content[0].Type != "output_text" {
					t.Fatalf("content = %#v", assistant.Content)
				}
			},
		},
		{
			name: "tool calls become function_call items with string arguments",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleAssistant,
					Content: []core.ContentBlock{{
						Type:       core.ContentTypeToolUse,
						ToolCallID: "call_1",
						ToolName:   "read",
						Input:      json.RawMessage(`{"path":"a.txt"}`),
					}},
				})
			},
			check: func(t *testing.T, payload *responsesRequest) {
				call := payload.Input[1]
				if call.Type != "function_call" || call.CallID != "call_1" || call.Name != "read" {
					t.Fatalf("call = %#v", call)
				}
				if call.Arguments != `{"path":"a.txt"}` {
					t.Fatalf("arguments = %q", call.Arguments)
				}
			},
		},
		{
			name: "empty tool arguments become an empty object",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleAssistant,
					Content: []core.ContentBlock{{Type: core.ContentTypeToolUse, ToolCallID: "call_1", ToolName: "ping"}},
				})
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if payload.Input[1].Arguments != "{}" {
					t.Fatalf("arguments = %q", payload.Input[1].Arguments)
				}
			},
		},
		{
			name: "a reasoning block is replayed with its encrypted content",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleAssistant,
					Content: []core.ContentBlock{
						{Type: core.ContentTypeReasoning, EncryptedContent: "opaque-blob"},
						{Type: core.ContentTypeText, Text: "answer"},
					},
				})
			},
			check: func(t *testing.T, payload *responsesRequest) {
				// The base request contributes the leading user item, then the
				// reasoning item and the assistant message follow in block order.
				if len(payload.Input) != 3 {
					t.Fatalf("input = %#v", payload.Input)
				}
				reasoning := payload.Input[1]
				if reasoning.Type != "reasoning" || reasoning.EncryptedContent != "opaque-blob" {
					t.Fatalf("reasoning = %#v", reasoning)
				}
				if payload.Input[2].Type != "message" || payload.Input[2].Role != "assistant" {
					t.Fatalf("message = %#v", payload.Input[2])
				}
			}},
		{
			name: "a thinking block is dropped",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleAssistant,
					Content: []core.ContentBlock{{Type: core.ContentTypeThinking, Thinking: "t", ThinkingSig: "sig"}},
				})
			},
			wantErr: "no convertible block",
		},
		{
			name: "tool results become function_call_output items",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleTool,
					Content: []core.ContentBlock{
						{Type: core.ContentTypeToolResult, ToolCallID: "call_1", Output: json.RawMessage(`"first"`)},
						{Type: core.ContentTypeToolResult, ToolCallID: "call_2", Output: json.RawMessage(`"second"`), IsError: true},
					},
				})
			},
			check: func(t *testing.T, payload *responsesRequest) {
				if len(payload.Input) != 3 {
					t.Fatalf("input = %#v", payload.Input)
				}
				first, second := payload.Input[1], payload.Input[2]
				if first.Type != "function_call_output" || first.CallID != "call_1" || first.Output != `"first"` {
					t.Fatalf("first output = %#v", first)
				}
				if second.CallID != "call_2" {
					t.Fatalf("second output = %#v", second)
				}
			},
		},
		{
			name: "a tool message without results is rejected",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleTool,
					Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "nope"}},
				})
			},
			wantErr: "tool message carries",
		},
		{
			name: "an unknown role is rejected",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Role = core.Role("moderator")
			},
			wantErr: "unsupported role",
		},
		{
			name: "an empty model is rejected",
			mutate: func(request *core.ChatRequest) {
				request.Model = ""
			},
			wantErr: "model must not be empty",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := base()
			if test.mutate != nil {
				test.mutate(request)
			}
			payload, err := responsesAdapterFor(t).BuildRequest(request)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			if test.check != nil {
				test.check(t, payload.(*responsesRequest))
			}
		})
	}
}

func TestResponsesBuildRequestRejectsNil(t *testing.T) {
	if _, err := responsesAdapterFor(t).BuildRequest(nil); err == nil {
		t.Fatal("nil request accepted")
	}
}

func TestResponsesThinkingMapping(t *testing.T) {
	adapter := responsesAdapterFor(t)
	cases := []struct {
		name   string
		config ThinkingConfig
		want   *responsesReasoning
	}{
		{name: "disabled", config: ThinkingConfig{Enabled: false, ReasoningEffort: "high"}},
		{
			name:   "explicit effort",
			config: ThinkingConfig{Enabled: true, ReasoningEffort: "MEDIUM"},
			want:   &responsesReasoning{Effort: "medium", Summary: "auto"},
		},
		{
			name:   "derived from a small budget",
			config: ThinkingConfig{Enabled: true, BudgetTokens: 1024},
			want:   &responsesReasoning{Effort: "low", Summary: "auto"},
		},
		{
			name:   "derived from a large budget",
			config: ThinkingConfig{Enabled: true, BudgetTokens: 32000},
			want:   &responsesReasoning{Effort: "high", Summary: "auto"},
		},
		{
			name:   "no budget at all",
			config: ThinkingConfig{Enabled: true},
			want:   &responsesReasoning{Effort: "low", Summary: "auto"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			payload := &responsesRequest{}
			if err := adapter.ApplyThinking(payload, test.config); err != nil {
				t.Fatal(err)
			}
			if test.want == nil {
				if payload.Reasoning != nil {
					t.Fatalf("reasoning = %#v", payload.Reasoning)
				}
				return
			}
			if payload.Reasoning == nil || *payload.Reasoning != *test.want {
				t.Fatalf("reasoning = %#v", payload.Reasoning)
			}
		})
	}
	if err := adapter.ApplyThinking(map[string]any{}, ThinkingConfig{}); err == nil {
		t.Fatal("foreign payload accepted")
	}
}

func TestResponsesChainLifecycle(t *testing.T) {
	adapter := responsesAdapterFor(t)
	request := &core.ChatRequest{Model: "gpt-4.1", Messages: []core.Message{
		textMessage(core.RoleUser, "1", "hi"),
	}}
	build := func() *responsesRequest {
		payload, err := adapter.BuildRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		return payload.(*responsesRequest)
	}
	if previous, resumable := adapter.ChainUsage(request); resumable || previous != "" {
		t.Fatalf("chain is live before it is enabled: %q %v", previous, resumable)
	}
	adapter.EnableServerState(true)
	if payload := build(); payload.PreviousResponseID != "" {
		t.Fatalf("previous response id = %q", payload.PreviousResponseID)
	}
	adapter.RecordChain("resp_1")
	if previous, resumable := adapter.ChainUsage(request); !resumable || previous != "resp_1" {
		t.Fatalf("chain = %q %v", previous, resumable)
	}
	resumed := build()
	if resumed.PreviousResponseID != "resp_1" {
		t.Fatalf("previous response id = %q", resumed.PreviousResponseID)
	}
	adapter.InvalidateChain("history rewritten")
	if _, resumable := adapter.ChainUsage(request); resumable {
		t.Fatal("chain survived invalidation")
	}
	adapter.EnableServerState(false)
	adapter.RecordChain("resp_2")
	if _, resumable := adapter.ChainUsage(request); resumable {
		t.Fatal("chain survived being disabled")
	}
	if adapter.SentItems() != len(resumed.Input) {
		t.Fatalf("sent items = %d", adapter.SentItems())
	}
}

func TestResponsesUsageAndCacheStats(t *testing.T) {
	adapter := responsesAdapterFor(t)
	usage := adapter.NormalizeUsage(map[string]any{
		"input_tokens":         float64(50),
		"output_tokens":        float64(12),
		"input_tokens_details": map[string]any{"cached_tokens": float64(25)},
	})
	if usage.InputTokens != 50 || usage.OutputTokens != 12 || usage.CacheReadTokens != 25 {
		t.Fatalf("usage = %#v", usage)
	}
	if got := adapter.NormalizeUsage(nil); got != (Usage{}) {
		t.Fatalf("nil usage = %#v", got)
	}
	stats := adapter.ExtractCacheStats(usage)
	if stats.ReadTokens != 25 || stats.InputTokens != 50 {
		t.Fatalf("stats = %#v", stats)
	}
	if err := adapter.ApplyCacheHints(nil, CacheHints{}); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	adapter.usage = Usage{OutputTokens: 3}
	adapter.mu.Unlock()
	if got := adapter.TakeUsage(); got.OutputTokens != 3 {
		t.Fatalf("usage = %#v", got)
	}
	if got := adapter.TakeUsage(); got != (Usage{}) {
		t.Fatalf("usage was not drained: %#v", got)
	}
}

type responsesEventCase struct {
	name string
	data string
}

func responsesStream(t *testing.T, events ...responsesEventCase) string {
	t.Helper()
	var builder strings.Builder
	writer := NewStreamWriter(&builder)
	for _, event := range events {
		if err := writer.WriteEvent("", json.RawMessage(event.data)); err != nil {
			t.Fatal(err)
		}
	}
	return builder.String()
}

func drainResponses(t *testing.T, adapter *openAIResponsesAdapter, body string) []core.StreamChunk {
	t.Helper()
	chunks := make([]core.StreamChunk, 0, 8)
	for chunk := range adapter.ParseStream(strings.NewReader(body)) {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func TestResponsesStreamTextAndResponseID(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.created","response":{"id":"resp_1"}}`},
		responsesEventCase{data: `{"type":"response.output_text.delta","delta":"Hel"}`},
		responsesEventCase{data: `{"type":"response.output_text.delta","delta":""}`},
		responsesEventCase{data: `{"type":"response.output_text.delta","delta":"lo"}`},
		responsesEventCase{data: `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":8,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Text != "Hel" || chunks[1].Text != "lo" {
		t.Fatalf("text = %#v", chunks)
	}
	if chunks[2].Type != core.StreamTypeDone || chunks[2].ResponseID != "resp_1" {
		t.Fatalf("terminal = %#v", chunks[2])
	}
	if usage := adapter.TakeUsage(); usage.InputTokens != 8 || usage.CacheReadTokens != 4 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesStreamAggregatesFunctionCall(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`},
		responsesEventCase{data: `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"path\":"}`},
		responsesEventCase{data: `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"a.txt\"}"}`},
		responsesEventCase{data: `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","arguments":"{\"path\":\"a.txt\"}"}}`},
		responsesEventCase{data: `{"type":"response.completed","response":{"id":"resp_2"}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	call := chunks[0].ToolCall
	if call == nil || call.ToolCallID != "call_1" || call.ToolName != "read" {
		t.Fatalf("tool call = %#v", call)
	}
	if string(call.Input) != `{"path":"a.txt"}` {
		t.Fatalf("input = %s", call.Input)
	}
}

func TestResponsesStreamFallsBackToItemIDForCallID(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_9","name":"ping","arguments":"{}"}}`},
		responsesEventCase{data: `{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","arguments":"{}"}}`},
		responsesEventCase{data: `{"type":"response.completed","response":{"id":"resp_1"}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 2 || chunks[0].ToolCall.ToolCallID != "fc_9" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamHandlesDeltaWithoutItem(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{}"}`},
		responsesEventCase{data: `{"type":"response.completed","response":{"id":"resp_1"}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 2 || chunks[0].ToolCall == nil {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamRejectsMalformedArguments(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`},
		responsesEventCase{data: `{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"path\":"}`},
		responsesEventCase{data: `{"type":"response.completed","response":{"id":"resp_1"}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError {
		t.Fatalf("chunks = %#v", chunks)
	}
	if !strings.Contains(chunks[0].Err.Error(), "malformed arguments") {
		t.Fatalf("error = %v", chunks[0].Err)
	}
}

func TestResponsesStreamSurfacesReasoning(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.reasoning_summary_text.delta","delta":"pondering"}`},
		responsesEventCase{data: `{"type":"response.reasoning_text.delta","delta":" more"}`},
		responsesEventCase{data: `{"type":"response.completed","response":{"id":"resp_1"}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Type != core.StreamTypeThinking || chunks[1].Text != " more" {
		t.Fatalf("reasoning = %#v", chunks[:2])
	}
}

func TestResponsesStreamIgnoresPartEvents(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.content_part.added"}`},
		responsesEventCase{data: `{"type":"response.content_part.done"}`},
		responsesEventCase{data: `{"type":"response.output_text.done"}`},
		responsesEventCase{data: `{"type":"response.reasoning_summary_part.added"}`},
		responsesEventCase{data: `{"type":"response.reasoning_summary_part.done"}`},
		responsesEventCase{data: `{"type":"response.reasoning_summary_text.done"}`},
		responsesEventCase{data: `{"type":"response.in_progress","response":{"id":"resp_1"}}`},
		responsesEventCase{data: `{"type":"response.completed","response":{"id":"resp_1"}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamReportsFailures(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.failed","error":{"type":"server_error","message":"upstream down"}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError || chunks[0].Text != "upstream down" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamReportsUnspecifiedFailure(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t, responsesEventCase{data: `{"type":"error"}`})
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 1 || !strings.Contains(chunks[0].Text, "unspecified") {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamRejectsUnknownEvent(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t, responsesEventCase{data: `{"type":"response.mystery"}`})
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 1 || !strings.Contains(chunks[0].Err.Error(), "unsupported") {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamRejectsBrokenJSON(t *testing.T) {
	adapter := responsesAdapterFor(t)
	chunks := drainResponses(t, adapter, "data: {\"type\":\n\n")
	if len(chunks) != 1 || !strings.Contains(chunks[0].Err.Error(), "decode") {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamIgnoresEmptyPayloads(t *testing.T) {
	adapter := responsesAdapterFor(t)
	chunks := drainResponses(t, adapter, "data: \n\ndata: \n\n")
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamFlushesUnfinishedCall(t *testing.T) {
	adapter := responsesAdapterFor(t)
	body := responsesStream(t,
		responsesEventCase{data: `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\""}}`},
	)
	chunks := drainResponses(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestResponsesStreamStopsWhenConsumerLeaves(t *testing.T) {
	adapter := responsesAdapterFor(t)
	var builder strings.Builder
	writer := NewStreamWriter(&builder)
	for index := 0; index < 8; index++ {
		if err := writer.WriteEvent("", json.RawMessage(
			`{"type":"response.output_text.delta","delta":"x"}`)); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	for range adapter.ParseStream(strings.NewReader(builder.String())) {
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Fatalf("consumed %d chunks", seen)
	}
}

func TestResponsesGatewayEndToEnd(t *testing.T) {
	var received responsesRequest
	var path string
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		headers = request.Header.Clone()
		path = request.URL.Path
		body, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		recorder := newResponseRecorder(writer)
		recorder.event(`{"type":"response.created","response":{"id":"resp_7"}}`)
		recorder.event(`{"type":"response.output_text.delta","delta":"hello"}`)
		recorder.event(`{"type":"response.completed","response":{"id":"resp_7","usage":{"input_tokens":6,"output_tokens":1}}}`)
	}))
	defer server.Close()

	gateway, err := New(Config{
		Format:   FormatOpenAIResponses,
		Endpoint: server.URL + "/v1",
		APIKey:   "test-key",
		Model:    "gpt-4.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:      "gpt-4.1",
		SystemHint: "be terse",
		Tools:      []core.ToolSpec{toolSpec("read")},
		Messages:   []core.Message{textMessage(core.RoleUser, "1", "hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	var responseID string
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if chunk.Type == core.StreamTypeText {
			texts = append(texts, chunk.Text)
		}
		if chunk.ResponseID != "" {
			responseID = chunk.ResponseID
		}
	}
	if len(texts) != 1 || texts[0] != "hello" {
		t.Fatalf("texts = %#v", texts)
	}
	if responseID != "resp_7" {
		t.Fatalf("response id = %q", responseID)
	}
	if path != "/v1/responses" {
		t.Fatalf("path = %q", path)
	}
	if headers.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("authorization = %q", headers.Get("Authorization"))
	}
	if received.Instructions != "be terse" || len(received.Tools) != 1 || !received.Store {
		t.Fatalf("request = %#v", received)
	}
	if usage := gateway.Usage(); usage.InputTokens != 6 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesGatewayResumesTheChain(t *testing.T) {
	var requests []responsesRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var received responsesRequest
		body, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		requests = append(requests, received)
		recorder := newResponseRecorder(writer)
		recorder.event(`{"type":"response.completed","response":{"id":"resp_9"}}`)
	}))
	defer server.Close()

	gateway, err := New(Config{
		Format:      FormatOpenAIResponses,
		Endpoint:    server.URL + "/v1",
		APIKey:      "k",
		Model:       "gpt-4.1",
		ServerState: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	assistant := core.Message{
		ID:   "2",
		Role: core.RoleAssistant,
		Content: []core.ContentBlock{
			{Type: core.ContentTypeReasoning, EncryptedContent: "blob"},
			{Type: core.ContentTypeText, Text: "answer"},
		},
	}
	assistant.Metadata = map[string]any{"response_id": "resp_9"}
	history := []core.Message{
		textMessage(core.RoleUser, "1", "hi"),
		assistant,
		textMessage(core.RoleUser, "3", "next"),
	}
	for attempt := 0; attempt < 2; attempt++ {
		stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{Model: "gpt-4.1", Messages: history})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	if requests[0].PreviousResponseID != "" {
		t.Fatalf("first request resumed a chain: %q", requests[0].PreviousResponseID)
	}
	if requests[1].PreviousResponseID != "resp_9" {
		t.Fatalf("second request previous id = %q", requests[1].PreviousResponseID)
	}
	if len(requests[1].Input) != 1 || requests[1].Input[0].Role != "user" {
		t.Fatalf("resumed input = %#v", requests[1].Input)
	}
}

func TestResponsesGatewayFallsBackWhenTheChainDiverges(t *testing.T) {
	var requests []responsesRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var received responsesRequest
		body, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		requests = append(requests, received)
		recorder := newResponseRecorder(writer)
		recorder.event(`{"type":"response.completed","response":{"id":"resp_5"}}`)
	}))
	defer server.Close()

	gateway, err := New(Config{
		Format:      FormatOpenAIResponses,
		Endpoint:    server.URL + "/v1",
		APIKey:      "k",
		Model:       "gpt-4.1",
		ServerState: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	assistant := core.Message{ID: "2", Role: core.RoleAssistant, Content: []core.ContentBlock{
		{Type: core.ContentTypeText, Text: "answer"},
	}}
	assistant.Metadata = map[string]any{"response_id": "resp_5"}
	first := []core.Message{textMessage(core.RoleUser, "1", "hi"), assistant}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{Model: "gpt-4.1", Messages: first})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	// The history no longer contains the assistant turn the chain points at,
	// which is what a compaction or an edit looks like from the gateway.
	rewritten := []core.Message{textMessage(core.RoleUser, "0", "different"), textMessage(core.RoleUser, "1", "hi")}
	stream, err = gateway.ChatStream(t.Context(), core.ChatRequest{Model: "gpt-4.1", Messages: rewritten})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	if requests[1].PreviousResponseID != "" {
		t.Fatalf("diverged history still resumed: %q", requests[1].PreviousResponseID)
	}
	if len(requests[1].Input) != 2 {
		t.Fatalf("fallback input = %#v", requests[1].Input)
	}
}

func TestResponsesGatewayPropagatesHTTPErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(writer, `{"error":{"message":"bad model"}}`)
	}))
	defer server.Close()
	gateway, err := New(Config{Format: FormatOpenAIResponses, Endpoint: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:    "gpt-4.1",
		Messages: []core.Message{textMessage(core.RoleUser, "1", "hi")},
	}); err == nil {
		t.Fatal("provider error accepted")
	} else if !strings.Contains(err.Error(), "bad model") {
		t.Fatalf("error = %v", err)
	}
}

func TestResponsesGatewayReplaysEncryptedReasoning(t *testing.T) {
	var received responsesRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("decode body: %v", err)
			return
		}
		recorder := newResponseRecorder(writer)
		recorder.event(`{"type":"response.completed","response":{"id":"resp_1"}}`)
	}))
	defer server.Close()
	gateway, err := New(Config{Format: FormatOpenAIResponses, Endpoint: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model: "gpt-4.1",
		Messages: []core.Message{
			textMessage(core.RoleUser, "1", "hi"),
			{
				ID:   "2",
				Role: core.RoleAssistant,
				Content: []core.ContentBlock{
					{Type: core.ContentTypeReasoning, EncryptedContent: "opaque-blob"},
					{Type: core.ContentTypeText, Text: "answer"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if len(received.Input) != 3 || received.Input[1].Type != "reasoning" ||
		received.Input[1].EncryptedContent != "opaque-blob" {
		t.Fatalf("input = %#v", received.Input)
	}
}
