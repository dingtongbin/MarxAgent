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

func anthropicAdapterFor(t *testing.T) *anthropicMessagesAdapter {
	t.Helper()
	adapter, err := NewAdapter(FormatAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	concrete, ok := adapter.(*anthropicMessagesAdapter)
	if !ok {
		t.Fatalf("adapter = %T", adapter)
	}
	return concrete
}

func buildAnthropic(t *testing.T, request *core.ChatRequest) *anthropicRequest {
	t.Helper()
	payload, err := anthropicAdapterFor(t).BuildRequest(request)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	converted, ok := payload.(*anthropicRequest)
	if !ok {
		t.Fatalf("payload = %T", payload)
	}
	return converted
}

func anthropicUser(text string) core.Message {
	return textMessage(core.RoleUser, "1", text)
}

func TestAnthropicBuildRequestTable(t *testing.T) {
	base := func() *core.ChatRequest {
		return &core.ChatRequest{Model: "claude-sonnet-4-5", Messages: []core.Message{anthropicUser("hello")}}
	}
	cases := []struct {
		name    string
		mutate  func(*core.ChatRequest)
		check   func(*testing.T, *anthropicRequest)
		wantErr string
	}{
		{
			name: "plain conversation",
			check: func(t *testing.T, payload *anthropicRequest) {
				if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" {
					t.Fatalf("messages = %#v", payload.Messages)
				}
				if len(payload.Messages[0].Content) != 1 || payload.Messages[0].Content[0].Text != "hello" {
					t.Fatalf("content = %#v", payload.Messages[0].Content)
				}
				if !payload.Stream || payload.System != nil {
					t.Fatalf("stream = %v system = %#v", payload.Stream, payload.System)
				}
			},
		},
		{
			name: "max tokens is required and defaults to one",
			check: func(t *testing.T, payload *anthropicRequest) {
				if payload.MaxTokens != 1 {
					t.Fatalf("max tokens = %d", payload.MaxTokens)
				}
			},
		},
		{
			name: "max tokens is forwarded",
			mutate: func(request *core.ChatRequest) {
				request.MaxTokens = 2048
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				if payload.MaxTokens != 2048 {
					t.Fatalf("max tokens = %d", payload.MaxTokens)
				}
			},
		},
		{
			name: "system hint becomes the top level field",
			mutate: func(request *core.ChatRequest) {
				request.SystemHint = "be terse"
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				if payload.System != "be terse" {
					t.Fatalf("system = %#v", payload.System)
				}
				if len(payload.Messages) != 1 {
					t.Fatalf("system leaked into the history: %#v", payload.Messages)
				}
			},
		},
		{
			name: "hoisted system messages merge with the hint",
			mutate: func(request *core.ChatRequest) {
				request.SystemHint = "be terse"
				request.Messages = append([]core.Message{textMessage(core.RoleSystem, "s", "history")}, request.Messages...)
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				if payload.System != "be terse\n\nhistory" {
					t.Fatalf("system = %#v", payload.System)
				}
			},
		},
		{
			name: "temperature is forwarded only when set",
			mutate: func(request *core.ChatRequest) {
				request.Temperature = 0.2
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				if payload.Temperature == nil || *payload.Temperature != 0.2 {
					t.Fatalf("temperature = %v", payload.Temperature)
				}
			},
		},
		{
			name: "tools use input_schema",
			mutate: func(request *core.ChatRequest) {
				request.Tools = []core.ToolSpec{toolSpec("read")}
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				if len(payload.Tools) != 1 || payload.Tools[0].Name != "read" {
					t.Fatalf("tools = %#v", payload.Tools)
				}
				if string(payload.Tools[0].InputSchema) != `{"type":"object"}` {
					t.Fatalf("schema = %s", payload.Tools[0].InputSchema)
				}
			},
		},
		{
			name: "a tool without a schema gets an empty object schema",
			mutate: func(request *core.ChatRequest) {
				request.Tools = []core.ToolSpec{{Name: "ping", Description: "Ping."}}
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				if string(payload.Tools[0].InputSchema) != `{"type":"object","properties":{}}` {
					t.Fatalf("schema = %s", payload.Tools[0].InputSchema)
				}
			},
		},
		{
			name: "images become base64 sources",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeText, Text: "look"},
					imageBlock("aGk=", "png"),
				}
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				blocks := payload.Messages[0].Content
				if len(blocks) != 2 || blocks[1].Type != "image" || blocks[1].Source == nil {
					t.Fatalf("content = %#v", blocks)
				}
				if blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" ||
					blocks[1].Source.Data != "aGk=" {
					t.Fatalf("source = %#v", blocks[1].Source)
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
			name: "tool use carries an object input",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleAssistant,
					Content: []core.ContentBlock{{
						Type:       core.ContentTypeToolUse,
						ToolCallID: "toolu_1",
						ToolName:   "read",
						Input:      json.RawMessage(`{"path":"a.txt"}`),
					}},
				})
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				assistant := payload.Messages[1]
				if assistant.Role != "assistant" || len(assistant.Content) != 1 {
					t.Fatalf("assistant = %#v", assistant)
				}
				call := assistant.Content[0]
				if call.Type != "tool_use" || call.ID != "toolu_1" || call.Name != "read" {
					t.Fatalf("tool use = %#v", call)
				}
				if string(call.Input) != `{"path":"a.txt"}` {
					t.Fatalf("input = %s", call.Input)
				}
			},
		},
		{
			name: "empty tool input becomes an empty object",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleAssistant,
					Content: []core.ContentBlock{{Type: core.ContentTypeToolUse, ToolCallID: "toolu_1", ToolName: "ping"}},
				})
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				if string(payload.Messages[1].Content[0].Input) != "{}" {
					t.Fatalf("input = %s", payload.Messages[1].Content[0].Input)
				}
			},
		},
		{
			name: "a signed thinking block is returned verbatim",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleAssistant,
					Content: []core.ContentBlock{
						{Type: core.ContentTypeThinking, Thinking: "reasoning", ThinkingSig: "sig-abc"},
						{Type: core.ContentTypeToolUse, ToolCallID: "toolu_1", ToolName: "read", Input: json.RawMessage(`{}`)},
					},
				})
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				blocks := payload.Messages[1].Content
				if len(blocks) != 2 || blocks[0].Type != "thinking" {
					t.Fatalf("content = %#v", blocks)
				}
				if blocks[0].Thinking != "reasoning" || blocks[0].Signature != "sig-abc" {
					t.Fatalf("thinking = %#v", blocks[0])
				}
				if blocks[1].Type != "tool_use" {
					t.Fatalf("thinking must precede the first tool use: %#v", blocks)
				}
			},
		},
		{
			name: "an unsigned thinking block is dropped",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleAssistant,
					Content: []core.ContentBlock{{Type: core.ContentTypeThinking, Thinking: "no signature"}},
				})
			},
			wantErr: "no convertible block",
		},
		{
			name: "a reasoning block cannot be replayed",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleAssistant,
					Content: []core.ContentBlock{{Type: core.ContentTypeReasoning, EncryptedContent: "opaque"}},
				})
			},
			wantErr: "cannot be sent in an assistant message",
		},
		{
			name: "tool results become a user message with tool_result blocks",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleTool,
					Content: []core.ContentBlock{
						{Type: core.ContentTypeToolResult, ToolCallID: "toolu_1", ToolName: "read", Output: json.RawMessage(`"first"`)},
						{Type: core.ContentTypeToolResult, ToolCallID: "toolu_2", ToolName: "read", Output: json.RawMessage(`"second"`), IsError: true},
					},
				})
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				if len(payload.Messages) != 2 {
					t.Fatalf("messages = %#v", payload.Messages)
				}
				results := payload.Messages[1]
				if results.Role != "user" || len(results.Content) != 2 {
					t.Fatalf("results = %#v", results)
				}
				first, second := results.Content[0], results.Content[1]
				if first.Type != "tool_result" || first.ToolUseID != "toolu_1" || first.Content != `"first"` {
					t.Fatalf("first result = %#v", first)
				}
				if !second.IsError || second.ToolUseID != "toolu_2" {
					t.Fatalf("second result = %#v", second)
				}
			},
		},
		{
			name: "a tool result may be inlined in a user message",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeText, Text: "here"},
					{Type: core.ContentTypeToolResult, ToolCallID: "toolu_1", Output: json.RawMessage(`"x"`)},
				}
			},
			check: func(t *testing.T, payload *anthropicRequest) {
				blocks := payload.Messages[0].Content
				if len(blocks) != 2 || blocks[1].Type != "tool_result" {
					t.Fatalf("content = %#v", blocks)
				}
			},
		},
		{
			name: "a tool block cannot be sent in a user message",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeToolUse, ToolCallID: "toolu_1", ToolName: "read"},
				}
			},
			wantErr: "cannot be sent in a user message",
		},
		{
			name: "a user message with no convertible block is rejected",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeThinking, ThinkingSig: "sig"},
				}
			},
			wantErr: "cannot be sent in a user message",
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
			payload, err := anthropicAdapterFor(t).BuildRequest(request)
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
				test.check(t, payload.(*anthropicRequest))
			}
		})
	}
}

func TestAnthropicBuildRequestRejectsNil(t *testing.T) {
	if _, err := anthropicAdapterFor(t).BuildRequest(nil); err == nil {
		t.Fatal("nil request accepted")
	}
}

func TestAnthropicToolMessageWithoutResultsIsRejected(t *testing.T) {
	message := core.Message{ID: "1", Role: core.RoleTool, Content: []core.ContentBlock{
		{Type: core.ContentTypeText, Text: "not a result"},
	}}
	if _, err := anthropicMessages(message); err == nil {
		t.Fatal("tool message without a result accepted")
	}
	empty := core.Message{ID: "2", Role: core.RoleTool}
	if _, err := anthropicMessages(empty); err == nil {
		t.Fatal("empty tool message accepted")
	}
}

func TestAnthropicThinkingWidensMaxTokens(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	cases := []struct {
		name          string
		config        ThinkingConfig
		requestTokens int
		wantEnabled   bool
		wantTokens    int
		wantErr       string
	}{
		{name: "disabled clears the block", config: ThinkingConfig{Enabled: false, BudgetTokens: 1024}},
		{name: "enabled with a budget", config: ThinkingConfig{Enabled: true, BudgetTokens: 4096}, wantEnabled: true, wantTokens: 8192},
		{
			name:          "an existing larger bound is kept",
			config:        ThinkingConfig{Enabled: true, BudgetTokens: 4096},
			requestTokens: 32000,
			wantEnabled:   true,
			wantTokens:    32000,
		},
		{name: "enabled without a budget", config: ThinkingConfig{Enabled: true}, wantErr: "positive budget"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			payload := &anthropicRequest{MaxTokens: test.requestTokens}
			if payload.MaxTokens == 0 {
				payload.MaxTokens = 1
			}
			err := adapter.ApplyThinking(payload, test.config)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !test.wantEnabled {
				if payload.Thinking != nil {
					t.Fatalf("thinking = %#v", payload.Thinking)
				}
				return
			}
			if payload.Thinking == nil || payload.Thinking.Type != "enabled" ||
				payload.Thinking.BudgetTokens != test.config.BudgetTokens {
				t.Fatalf("thinking = %#v", payload.Thinking)
			}
			if payload.MaxTokens != test.wantTokens {
				t.Fatalf("max tokens = %d, want %d", payload.MaxTokens, test.wantTokens)
			}
		})
	}
	if err := adapter.ApplyThinking(map[string]any{}, ThinkingConfig{}); err == nil {
		t.Fatal("foreign payload accepted")
	}
}

func TestAnthropicUsageNormalization(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	usage := adapter.NormalizeUsage(map[string]any{
		"input_tokens":                float64(40),
		"output_tokens":               float64(9),
		"cache_read_input_tokens":     float64(30),
		"cache_creation_input_tokens": float64(10),
	})
	if usage.InputTokens != 40 || usage.OutputTokens != 9 || usage.CacheReadTokens != 30 || usage.CacheWriteTokens != 10 {
		t.Fatalf("usage = %#v", usage)
	}
	if got := adapter.NormalizeUsage(nil); got != (Usage{}) {
		t.Fatalf("nil usage = %#v", got)
	}
	stats := adapter.ExtractCacheStats(usage)
	if stats.ReadTokens != 30 || stats.WriteTokens != 10 || stats.InputTokens != 40 {
		t.Fatalf("stats = %#v", stats)
	}
}

type anthropicEvent struct {
	name string
	data string
}

// anthropicStream builds a corpus in the given order. Server-sent events are
// ordered, so the tests must not rely on map iteration.
func anthropicStream(t *testing.T, events ...anthropicEvent) string {
	t.Helper()
	var builder strings.Builder
	writer := NewStreamWriter(&builder)
	for _, event := range events {
		if err := writer.WriteEvent(event.name, json.RawMessage(event.data)); err != nil {
			t.Fatal(err)
		}
	}
	return builder.String()
}

func drainAnthropic(t *testing.T, adapter *anthropicMessagesAdapter, body string) []core.StreamChunk {
	t.Helper()
	chunks := make([]core.StreamChunk, 0, 8)
	for chunk := range adapter.ParseStream(strings.NewReader(body)) {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func TestAnthropicStreamTextAndUsage(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":12,"cache_read_input_tokens":8}}}`},
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		anthropicEvent{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
		anthropicEvent{"content_block_delta_2", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		anthropicEvent{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`},
		anthropicEvent{"message_stop", `{"type":"message_stop"}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Text != "Hel" || chunks[1].Text != "lo" || chunks[2].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
	usage := adapter.TakeUsage()
	if usage.InputTokens != 12 || usage.OutputTokens != 2 || usage.CacheReadTokens != 8 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAnthropicStreamAggregatesToolInput(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`},
		anthropicEvent{"delta_a", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`},
		anthropicEvent{"delta_b", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		anthropicEvent{"message_stop", `{"type":"message_stop"}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	call := chunks[0].ToolCall
	if call == nil || call.ToolCallID != "toolu_1" || call.ToolName != "read" {
		t.Fatalf("tool call = %#v", call)
	}
	if string(call.Input) != `{"path":"a.txt"}` {
		t.Fatalf("input = %s", call.Input)
	}
	if chunks[1].Type != core.StreamTypeDone {
		t.Fatalf("terminal = %#v", chunks[1])
	}
}

func TestAnthropicStreamKeepsToolInputFromStart(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"ping","input":{"a":1}}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 2 || string(chunks[0].ToolCall.Input) != `{"a":1}` {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamEmptyToolInputBecomesObject(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"ping"}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 2 || string(chunks[0].ToolCall.Input) != "{}" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamRejectsMalformedToolInput(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`},
		anthropicEvent{"delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError {
		t.Fatalf("chunks = %#v", chunks)
	}
	if !strings.Contains(chunks[0].Err.Error(), "malformed input") {
		t.Fatalf("error = %v", chunks[0].Err)
	}
}

func TestAnthropicStreamCarriesSignature(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		anthropicEvent{"delta_a", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"pondering"}}`},
		anthropicEvent{"delta_b", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-xyz"}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		anthropicEvent{"message_stop", `{"type":"message_stop"}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Type != core.StreamTypeThinking || chunks[0].Text != "pondering" {
		t.Fatalf("thinking chunk = %#v", chunks[0])
	}
	if chunks[1].ThinkingSig != "sig-xyz" {
		t.Fatalf("signature chunk = %#v", chunks[1])
	}
}

func TestAnthropicStreamSignatureFromBlockStart(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"ready","signature":"sig-1"}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 2 || chunks[0].Text != "ready" || chunks[0].ThinkingSig != "sig-1" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamEmptyThinkingIsDropped(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		anthropicEvent{"message_stop", `{"type":"message_stop"}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamDeltaWithoutStart(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"orphan"}}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 2 || chunks[0].Text != "orphan" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamFlushesUnfinishedBlocksAtEnd(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	// The deltas arrive on the wire before the blocks close, and a block that was
	// never closed is released at the end in index order.
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`},
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
		anthropicEvent{"delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"streamed"}}`},
		anthropicEvent{"held", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Text != "streamed" {
		t.Fatalf("streamed chunk = %#v", chunks[0])
	}
	if chunks[1].Type != core.StreamTypeDone {
		t.Fatalf("terminal = %#v", chunks[1])
	}
}

func TestAnthropicStreamIgnoresPing(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"ping", `{"type":"ping"}`},
		anthropicEvent{"message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`},
		anthropicEvent{"message_stop", `{"type":"message_stop"}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamReportsErrors(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"error", `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError || chunks[0].Text != "overloaded" {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamReportsUnspecifiedError(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"error", `{"type":"error","error":{}}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 1 || !strings.Contains(chunks[0].Text, "unspecified") {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamRejectsUnknownEvent(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"weird", `{"type":"mystery"}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 1 || !strings.Contains(chunks[0].Err.Error(), "unsupported") {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamRejectsBrokenJSON(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	chunks := drainAnthropic(t, adapter, "data: {\"type\":\n\n")
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError {
		t.Fatalf("chunks = %#v", chunks)
	}
	if !strings.Contains(chunks[0].Err.Error(), "decode") {
		t.Fatalf("error = %v", chunks[0].Err)
	}
}

func TestAnthropicStreamUsesEventNameWhenTypeIsMissing(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	body := anthropicStream(t,
		anthropicEvent{"message_stop", `{}`},
	)
	chunks := drainAnthropic(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamIgnoresEmptyPayloads(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	chunks := drainAnthropic(t, adapter, "data: \n\ndata: \n\n")
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestAnthropicStreamStopsWhenConsumerLeaves(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	var builder strings.Builder
	writer := NewStreamWriter(&builder)
	for index := 0; index < 8; index++ {
		if err := writer.WriteEvent("content_block_delta", json.RawMessage(
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`)); err != nil {
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

func TestAnthropicSignatureSurvivesARoundTrip(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	signature := "EqQBCkYIBBgCKkDx0Op7q5r+2sQ0h2Q0h1AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	body := anthropicStream(t,
		anthropicEvent{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`},
		anthropicEvent{"delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`},
		anthropicEvent{"signature", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"` + signature + `"}}`},
		anthropicEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
		anthropicEvent{"tool_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`},
		anthropicEvent{"tool_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.txt\"}"}}`},
		anthropicEvent{"tool_stop", `{"type":"content_block_stop","index":1}`},
		anthropicEvent{"message_stop", `{"type":"message_stop"}`},
	)
	chunks := drainAnthropic(t, adapter, body)

	// Rebuild the universal message the L1 accumulator would have produced.
	rebuilt := core.Message{ID: "1", Role: core.RoleAssistant}
	accumulated := ""
	sig := ""
	for _, chunk := range chunks {
		switch {
		case chunk.Type == core.StreamTypeThinking:
			accumulated += chunk.Text
			if chunk.ThinkingSig != "" {
				sig = chunk.ThinkingSig
			}
		case chunk.Type == core.StreamTypeToolCall:
			rebuilt.Content = append(rebuilt.Content, *chunk.ToolCall)
		}
	}
	rebuilt.Content = append([]core.ContentBlock{{
		Type:        core.ContentTypeThinking,
		Thinking:    accumulated,
		ThinkingSig: sig,
	}}, rebuilt.Content...)

	payload := buildAnthropic(t, &core.ChatRequest{Model: "claude-sonnet-4-5", Messages: []core.Message{rebuilt}})
	assistant := payload.Messages[0]
	if len(assistant.Content) != 2 {
		t.Fatalf("assistant content = %#v", assistant.Content)
	}
	if assistant.Content[0].Signature != signature {
		t.Fatalf("signature = %q, want %q", assistant.Content[0].Signature, signature)
	}
	if assistant.Content[0].Thinking != "let me think" {
		t.Fatalf("thinking = %q", assistant.Content[0].Thinking)
	}
	if assistant.Content[1].Type != "tool_use" || string(assistant.Content[1].Input) != `{"path":"a.txt"}` {
		t.Fatalf("tool use = %#v", assistant.Content[1])
	}
}

func TestAnthropicGatewayEndToEnd(t *testing.T) {
	var received anthropicRequest
	server := newAnthropicServer(t, func(writer *responseRecorder) {
		writer.named("message_start", `{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5}}}`)
		writer.named("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		writer.named("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
		writer.named("content_block_stop", `{"type":"content_block_stop","index":0}`)
		writer.named("message_stop", `{"type":"message_stop"}`)
	}, &received)
	defer server.Close()

	gateway, err := New(Config{
		Format:           FormatAnthropicMessages,
		Endpoint:         server.URL,
		APIKey:           "test-key",
		Model:            "claude-sonnet-4-5",
		AnthropicVersion: "2023-06-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:      "claude-sonnet-4-5",
		SystemHint: "be terse",
		Tools:      []core.ToolSpec{toolSpec("read")},
		Messages:   []core.Message{anthropicUser("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for chunk := range stream {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if chunk.Type == core.StreamTypeText {
			texts = append(texts, chunk.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "hi" {
		t.Fatalf("texts = %#v", texts)
	}
	if received.Model != "claude-sonnet-4-5" || !received.Stream || received.System != "be terse" {
		t.Fatalf("request = %#v", received)
	}
	if len(received.Tools) != 1 || received.Tools[0].Name != "read" {
		t.Fatalf("tools = %#v", received.Tools)
	}
}

func TestAnthropicGatewaySendsRequiredHeaders(t *testing.T) {
	var headers http.Header
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		headers = request.Header.Clone()
		path = request.URL.Path
		recorder := newResponseRecorder(writer)
		recorder.named("message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	gateway, err := New(Config{
		Format:           FormatAnthropicMessages,
		Endpoint:         server.URL + "/v1",
		APIKey:           "test-key",
		AnthropicVersion: "2099-01-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []core.Message{anthropicUser("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if path != "/v1/messages" {
		t.Fatalf("path = %q", path)
	}
	if headers.Get("x-api-key") != "test-key" {
		t.Fatalf("x-api-key = %q", headers.Get("x-api-key"))
	}
	if headers.Get("anthropic-version") != "2099-01-01" {
		t.Fatalf("anthropic-version = %q", headers.Get("anthropic-version"))
	}
	if headers.Get("Authorization") != "" {
		t.Fatalf("unexpected authorization header %q", headers.Get("Authorization"))
	}
}

func TestAnthropicGatewayThinkingWidensTheRequest(t *testing.T) {
	var received anthropicRequest
	server := newAnthropicServer(t, func(writer *responseRecorder) {
		writer.named("message_stop", `{"type":"message_stop"}`)
	}, &received)
	defer server.Close()

	gateway, err := New(Config{
		Format:   FormatAnthropicMessages,
		Endpoint: server.URL,
		APIKey:   "k",
		Model:    "claude-sonnet-4-5",
		Thinking: &ThinkingConfig{Enabled: true, BudgetTokens: 8192},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 4096,
		Messages:  []core.Message{anthropicUser("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if received.Thinking == nil || received.Thinking.BudgetTokens != 8192 {
		t.Fatalf("thinking = %#v", received.Thinking)
	}
	if received.MaxTokens != 16384 {
		t.Fatalf("max tokens = %d", received.MaxTokens)
	}
}

func TestAnthropicGatewayPropagatesHTTPErrors(t *testing.T) {
	server := newAnthropicServer(t, nil, nil)
	defer server.Close()
	gateway, err := New(Config{Format: FormatAnthropicMessages, Endpoint: server.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []core.Message{anthropicUser("hi")},
	}); err == nil {
		t.Fatal("provider error accepted")
	}
}

func newAnthropicServer(
	t *testing.T,
	handle func(*responseRecorder),
	received *anthropicRequest,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if received != nil {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
				return
			}
			if err := json.Unmarshal(body, received); err != nil {
				t.Errorf("decode body: %v", err)
				return
			}
		}
		if handle == nil {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		handle(newResponseRecorder(writer))
	}))
}
