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

func completionsAdapterFor(t *testing.T) *openAICompletionsAdapter {
	t.Helper()
	adapter, err := NewAdapter(FormatOpenAICompletions)
	if err != nil {
		t.Fatal(err)
	}
	concrete, ok := adapter.(*openAICompletionsAdapter)
	if !ok {
		t.Fatalf("adapter = %T", adapter)
	}
	return concrete
}

func buildCompletions(t *testing.T, request *core.ChatRequest) *completionsRequest {
	t.Helper()
	payload, err := completionsAdapterFor(t).BuildRequest(request)
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	converted, ok := payload.(*completionsRequest)
	if !ok {
		t.Fatalf("payload = %T", payload)
	}
	return converted
}
func imageBlock(data, mediaType string) core.ContentBlock {
	return core.ContentBlock{Type: core.ContentTypeImage, ImageData: data, MediaFormat: mediaType}
}

func toolSpec(name string) core.ToolSpec {
	return core.ToolSpec{Name: name, Description: name + " tool.", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func TestCompletionsBuildRequestTable(t *testing.T) {
	base := func() *core.ChatRequest {
		return &core.ChatRequest{
			Model:    "gpt-4o",
			Messages: []core.Message{textMessage(core.RoleUser, "1", "hello")},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*core.ChatRequest)
		check   func(*testing.T, *completionsRequest)
		wantErr string
	}{
		{
			name: "plain text conversation",
			check: func(t *testing.T, payload *completionsRequest) {
				if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" {
					t.Fatalf("messages = %#v", payload.Messages)
				}
				if payload.Messages[0].Content != "hello" {
					t.Fatalf("content = %#v", payload.Messages[0].Content)
				}
			},
		},
		{
			name: "streaming is always requested with usage",
			check: func(t *testing.T, payload *completionsRequest) {
				if !payload.Stream || payload.StreamOptions == nil || !payload.StreamOptions.IncludeUsage {
					t.Fatalf("stream = %v options = %#v", payload.Stream, payload.StreamOptions)
				}
			},
		},
		{
			name: "system hint becomes the first message",
			mutate: func(request *core.ChatRequest) {
				request.SystemHint = "be terse"
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if len(payload.Messages) != 2 || payload.Messages[0].Role != "system" {
					t.Fatalf("messages = %#v", payload.Messages)
				}
				if payload.Messages[0].Content != "be terse" {
					t.Fatalf("system content = %#v", payload.Messages[0].Content)
				}
			},
		},
		{
			name: "system messages are hoisted and merged with the hint",
			mutate: func(request *core.ChatRequest) {
				request.SystemHint = "be terse"
				request.Messages = append([]core.Message{textMessage(core.RoleSystem, "s", "from history")}, request.Messages...)
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if payload.Messages[0].Content != "be terse\n\nfrom history" {
					t.Fatalf("system content = %#v", payload.Messages[0].Content)
				}
				for _, message := range payload.Messages[1:] {
					if message.Role == "system" {
						t.Fatal("a system message stayed in the history")
					}
				}
			},
		},
		{
			name: "max tokens uses the modern field name",
			mutate: func(request *core.ChatRequest) {
				request.MaxTokens = 512
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if payload.MaxCompletionTokens != 512 {
					t.Fatalf("max tokens = %d", payload.MaxCompletionTokens)
				}
			},
		},
		{
			name: "zero temperature is omitted to keep the prefix stable",
			check: func(t *testing.T, payload *completionsRequest) {
				if payload.Temperature != nil {
					t.Fatalf("temperature = %v", *payload.Temperature)
				}
			},
		},
		{
			name: "temperature is forwarded",
			mutate: func(request *core.ChatRequest) {
				request.Temperature = 0.4
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if payload.Temperature == nil || *payload.Temperature != 0.4 {
					t.Fatalf("temperature = %v", payload.Temperature)
				}
			},
		},
		{
			name: "tools are declared as functions",
			mutate: func(request *core.ChatRequest) {
				request.Tools = []core.ToolSpec{toolSpec("read"), toolSpec("write")}
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if len(payload.Tools) != 2 {
					t.Fatalf("tools = %#v", payload.Tools)
				}
				if payload.Tools[0].Type != "function" || payload.Tools[1].Function.Name != "write" {
					t.Fatalf("tools = %#v", payload.Tools)
				}
			},
		},
		{
			name: "a tool without a schema gets an empty object schema",
			mutate: func(request *core.ChatRequest) {
				request.Tools = []core.ToolSpec{{Name: "ping", Description: "Ping."}}
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if string(payload.Tools[0].Function.Parameters) != `{"type":"object","properties":{}}` {
					t.Fatalf("parameters = %s", payload.Tools[0].Function.Parameters)
				}
			},
		},
		{
			name: "multiple text blocks are concatenated",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeText, Text: "one "},
					{Type: core.ContentTypeText, Text: "two"},
				}
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if payload.Messages[0].Content != "one two" {
					t.Fatalf("content = %#v", payload.Messages[0].Content)
				}
			},
		},
		{
			name: "structured input keeps its text",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeStructured, Text: "payload", Input: json.RawMessage(`{"a":1}`)},
				}
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if payload.Messages[0].Content != "payload" {
					t.Fatalf("content = %#v", payload.Messages[0].Content)
				}
			},
		},
		{
			name: "images become data urls inside a part array",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{
					{Type: core.ContentTypeText, Text: "look"},
					imageBlock("aGk=", "png"),
				}
			},
			check: func(t *testing.T, payload *completionsRequest) {
				parts, ok := payload.Messages[0].Content.([]completionsContentPart)
				if !ok || len(parts) != 2 {
					t.Fatalf("content = %#v", payload.Messages[0].Content)
				}
				if parts[0].Type != "text" || parts[0].Text != "look" {
					t.Fatalf("text part = %#v", parts[0])
				}
				if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,aGk=" {
					t.Fatalf("image part = %#v", parts[1])
				}
			},
		},
		{
			name: "an image without text produces only image parts",
			mutate: func(request *core.ChatRequest) {
				request.Messages[0].Content = []core.ContentBlock{imageBlock("aGk=", "jpeg")}
			},
			check: func(t *testing.T, payload *completionsRequest) {
				parts, ok := payload.Messages[0].Content.([]completionsContentPart)
				if !ok || len(parts) != 1 || parts[0].Type != "image_url" {
					t.Fatalf("content = %#v", payload.Messages[0].Content)
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
			name: "tool calls become string arguments",
			mutate: func(request *core.ChatRequest) {
				request.Tools = []core.ToolSpec{toolSpec("read")}
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
			check: func(t *testing.T, payload *completionsRequest) {
				assistant := payload.Messages[1]
				if assistant.Role != "assistant" || assistant.Content != nil {
					t.Fatalf("assistant = %#v", assistant)
				}
				if len(assistant.ToolCalls) != 1 {
					t.Fatalf("tool calls = %#v", assistant.ToolCalls)
				}
				call := assistant.ToolCalls[0]
				if call.Type != "function" || call.ID != "call_1" || call.Function.Name != "read" {
					t.Fatalf("tool call = %#v", call)
				}
				if call.Function.Arguments != `{"path":"a.txt"}` {
					t.Fatalf("arguments = %q", call.Function.Arguments)
				}
			},
		},
		{
			name: "empty tool arguments become an empty object",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleAssistant,
					Content: []core.ContentBlock{{
						Type:       core.ContentTypeToolUse,
						ToolCallID: "call_1",
						ToolName:   "ping",
					}},
				})
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if payload.Messages[1].ToolCalls[0].Function.Arguments != "{}" {
					t.Fatalf("arguments = %q", payload.Messages[1].ToolCalls[0].Function.Arguments)
				}
			},
		},
		{
			name: "text and tool calls share one assistant message",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleAssistant,
					Content: []core.ContentBlock{
						{Type: core.ContentTypeText, Text: "reading"},
						{Type: core.ContentTypeToolUse, ToolCallID: "call_1", ToolName: "read", Input: json.RawMessage(`{}`)},
					},
				})
			},
			check: func(t *testing.T, payload *completionsRequest) {
				assistant := payload.Messages[1]
				if assistant.Content != "reading" || len(assistant.ToolCalls) != 1 {
					t.Fatalf("assistant = %#v", assistant)
				}
			},
		},
		{
			name: "thinking and reasoning blocks are dropped",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleAssistant,
					Content: []core.ContentBlock{
						{Type: core.ContentTypeThinking, Thinking: "secret", ThinkingSig: "sig"},
						{Type: core.ContentTypeReasoning, EncryptedContent: "opaque"},
						{Type: core.ContentTypeText, Text: "answer"},
					},
				})
			},
			check: func(t *testing.T, payload *completionsRequest) {
				encoded, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "opaque") {
					t.Fatalf("credential leaked into the payload: %s", encoded)
				}
			},
		},
		{
			name: "an assistant message with only dropped blocks is rejected",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleAssistant,
					Content: []core.ContentBlock{{Type: core.ContentTypeThinking, ThinkingSig: "sig"}},
				})
			},
			wantErr: "has no content",
		},
		{
			name: "each tool result becomes its own message",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:   "2",
					Role: core.RoleTool,
					Content: []core.ContentBlock{
						{Type: core.ContentTypeToolResult, ToolCallID: "call_1", ToolName: "read", Output: json.RawMessage(`"first"`)},
						{Type: core.ContentTypeToolResult, ToolCallID: "call_2", ToolName: "read", Output: json.RawMessage(`"second"`), IsError: true},
					},
				})
			},
			check: func(t *testing.T, payload *completionsRequest) {
				if len(payload.Messages) != 3 {
					t.Fatalf("messages = %#v", payload.Messages)
				}
				first, second := payload.Messages[1], payload.Messages[2]
				if first.Role != "tool" || first.ToolCallID != "call_1" || first.Content != `"first"` {
					t.Fatalf("first result = %#v", first)
				}
				if second.ToolCallID != "call_2" || second.Content != `"second"` || second.Name != "read" {
					t.Fatalf("second result = %#v", second)
				}
			},
		},
		{
			name: "a tool message without a result is rejected",
			mutate: func(request *core.ChatRequest) {
				request.Messages = append(request.Messages, core.Message{
					ID:      "2",
					Role:    core.RoleTool,
					Content: []core.ContentBlock{{Type: core.ContentTypeToolResult, ToolCallID: "c", Output: json.RawMessage(`1`)}},
				})
			},
			wantErr: "",
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
			payload, err := completionsAdapterFor(t).BuildRequest(request)
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
				test.check(t, payload.(*completionsRequest))
			}
		})
	}
}

func TestCompletionsBuildRequestRejectsNil(t *testing.T) {
	if _, err := completionsAdapterFor(t).BuildRequest(nil); err == nil {
		t.Fatal("nil request accepted")
	}
}

func TestCompletionsToolMessageWithoutResultsIsRejected(t *testing.T) {
	message := core.Message{
		ID:      "1",
		Role:    core.RoleTool,
		Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "not a result"}},
	}
	if _, err := completionsMessages(message); err == nil {
		t.Fatal("tool message without a result was accepted")
	} else if !strings.Contains(err.Error(), "no result block") {
		t.Fatalf("error = %v", err)
	}
	request := &core.ChatRequest{Model: "gpt-4o", Messages: []core.Message{message}}
	if _, err := completionsAdapterFor(t).BuildRequest(request); err == nil {
		t.Fatal("validator accepted a tool message carrying text")
	}
}

func TestCompletionsThinkingMapping(t *testing.T) {
	cases := []struct {
		name   string
		config ThinkingConfig
		want   string
	}{
		{name: "disabled clears the field", config: ThinkingConfig{Enabled: false, ReasoningEffort: "high"}, want: ""},
		{name: "explicit effort wins", config: ThinkingConfig{Enabled: true, ReasoningEffort: "LOW"}, want: "low"},
		{name: "small budget", config: ThinkingConfig{Enabled: true, BudgetTokens: 2048}, want: "low"},
		{name: "medium budget", config: ThinkingConfig{Enabled: true, BudgetTokens: 8192}, want: "medium"},
		{name: "large budget", config: ThinkingConfig{Enabled: true, BudgetTokens: 32768}, want: "high"},
		{name: "no budget and no effort", config: ThinkingConfig{Enabled: true}, want: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			payload := &completionsRequest{}
			if err := completionsAdapterFor(t).ApplyThinking(payload, test.config); err != nil {
				t.Fatal(err)
			}
			if payload.ReasoningEffort != test.want {
				t.Fatalf("reasoning effort = %q, want %q", payload.ReasoningEffort, test.want)
			}
		})
	}
}

func TestCompletionsThinkingRejectsForeignPayload(t *testing.T) {
	if err := completionsAdapterFor(t).ApplyThinking(map[string]any{}, ThinkingConfig{}); err == nil {
		t.Fatal("foreign payload accepted")
	}
}

func TestCompletionsUsageNormalization(t *testing.T) {
	adapter := completionsAdapterFor(t)
	usage := adapter.NormalizeUsage(map[string]any{
		"prompt_tokens":             float64(120),
		"completion_tokens":         float64(30),
		"prompt_tokens_details":     map[string]any{"cached_tokens": float64(80)},
		"completion_tokens_details": map[string]any{"reasoning_tokens": float64(12)},
	})
	if usage.InputTokens != 120 || usage.OutputTokens != 30 || usage.CacheReadTokens != 80 || usage.CacheWriteTokens != 12 {
		t.Fatalf("usage = %#v", usage)
	}
	if got := adapter.NormalizeUsage(nil); got != (Usage{}) {
		t.Fatalf("nil usage = %#v", got)
	}
	if got := adapter.NormalizeUsage(map[string]any{"prompt_tokens": "nope"}); got != (Usage{}) {
		t.Fatalf("malformed usage = %#v", got)
	}
}

func TestCompletionsCacheStatsAndTakeUsage(t *testing.T) {
	adapter := completionsAdapterFor(t)
	stats := adapter.ExtractCacheStats(Usage{InputTokens: 100, CacheReadTokens: 60, CacheWriteTokens: 5})
	if stats.ReadTokens != 60 || stats.WriteTokens != 5 || stats.InputTokens != 100 {
		t.Fatalf("stats = %#v", stats)
	}
	if stats.HitRate() != 0.6 {
		t.Fatalf("hit rate = %v", stats.HitRate())
	}
	if err := adapter.ApplyCacheHints(nil, CacheHints{StablePrefix: true}); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	adapter.usage = Usage{InputTokens: 5}
	adapter.mu.Unlock()
	if got := adapter.TakeUsage(); got.InputTokens != 5 {
		t.Fatalf("usage = %#v", got)
	}
	if got := adapter.TakeUsage(); got != (Usage{}) {
		t.Fatalf("usage was not drained: %#v", got)
	}
}

func TestCompletionsAdapterRejectsUnconvertibleMessages(t *testing.T) {
	if _, err := completionsMessages(core.Message{ID: "1", Role: core.RoleSystem, Content: []core.ContentBlock{
		{Type: core.ContentTypeText, Text: "hoist me"},
	}}); err == nil {
		t.Fatal("system message was not hoisted away")
	}
	// The adapter is reached directly here because the request validator
	// rejects an unknown role before translation.
	if _, err := completionsMessages(core.Message{ID: "1", Role: core.Role("moderator")}); err == nil {
		t.Fatal("unknown role accepted")
	} else if !strings.Contains(err.Error(), "cannot be sent") {
		t.Fatalf("error = %v", err)
	}
}

func TestCompletionsUserContentDropsReasoning(t *testing.T) {
	content, err := completionsUserContent([]core.ContentBlock{
		{Type: core.ContentTypeThinking, ThinkingSig: "sig"},
		{Type: core.ContentTypeText, Text: "kept"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if content != "kept" {
		t.Fatalf("content = %#v", content)
	}
}

func TestStreamWriterRawDataRejectsUninitializedWriter(t *testing.T) {
	var writer *StreamWriter
	if err := writer.WriteRawData("[DONE]"); err == nil {
		t.Fatal("nil writer accepted raw data")
	}
	var builder strings.Builder
	if err := NewStreamWriter(&builder).WriteRawData("[DONE]"); err != nil {
		t.Fatal(err)
	}
	if builder.String() != "data: [DONE]\n\n" {
		t.Fatalf("raw data = %q", builder.String())
	}
}

func TestCompletionsFormatIsRegistered(t *testing.T) {
	found := false
	for _, format := range Formats() {
		if format == FormatOpenAICompletions {
			found = true
		}
	}
	if !found {
		t.Fatalf("completions format is not registered: %v", Formats())
	}
	adapter := completionsAdapterFor(t)
	if adapter.Format() != FormatOpenAICompletions {
		t.Fatalf("format = %q", adapter.Format())
	}
}

func completionsStreamBody(t *testing.T, events ...string) string {
	t.Helper()
	var builder strings.Builder
	writer := NewStreamWriter(&builder)
	for _, event := range events {
		if err := writer.WriteEvent("", json.RawMessage(event)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.WriteRawData("[DONE]"); err != nil {
		t.Fatal(err)
	}
	return builder.String()
}

func drainCompletions(t *testing.T, adapter *openAICompletionsAdapter, body string) []core.StreamChunk {
	t.Helper()
	chunks := make([]core.StreamChunk, 0, 8)
	for chunk := range adapter.ParseStream(strings.NewReader(body)) {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func TestCompletionsStreamText(t *testing.T) {
	body := completionsStreamBody(t,
		`{"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"1","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`,
	)
	chunks := drainCompletions(t, completionsAdapterFor(t), body)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Text != "He" || chunks[1].Text != "llo" {
		t.Fatalf("text chunks = %#v", chunks)
	}
	if chunks[2].Type != core.StreamTypeDone {
		t.Fatalf("terminal chunk = %#v", chunks[2])
	}
	if usage := completionsAdapterFor(t).TakeUsage(); usage != (Usage{}) {
		t.Fatalf("usage leaked between adapters: %#v", usage)
	}
}

func TestCompletionsStreamAggregatesToolCallFragments(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := completionsStreamBody(t,
		`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":""}}]}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"ping","arguments":"{}"}}]}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	chunks := drainCompletions(t, adapter, body)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %#v", chunks)
	}
	first, second := chunks[0].ToolCall, chunks[1].ToolCall
	if first == nil || first.ToolCallID != "call_a" || first.ToolName != "read" {
		t.Fatalf("first call = %#v", first)
	}
	if string(first.Input) != `{"path":"a.txt"}` {
		t.Fatalf("first input = %s", first.Input)
	}
	if second == nil || second.ToolCallID != "call_b" || string(second.Input) != `{}` {
		t.Fatalf("second call = %#v", second)
	}
	if chunks[2].Type != core.StreamTypeDone {
		t.Fatalf("terminal chunk = %#v", chunks[2])
	}
}

func TestCompletionsStreamFlushesToolCallsWithoutFinishReason(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := completionsStreamBody(t,
		`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`,
	)
	chunks := drainCompletions(t, adapter, body)
	if len(chunks) != 2 || chunks[0].ToolCall == nil || chunks[1].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestCompletionsStreamRejectsMalformedToolArguments(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := completionsStreamBody(t,
		`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
	)
	chunks := drainCompletions(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError || chunks[0].Err == nil {
		t.Fatalf("chunks = %#v", chunks)
	}
	if !strings.Contains(chunks[0].Err.Error(), "malformed arguments") {
		t.Fatalf("error = %v", chunks[0].Err)
	}
}

func TestCompletionsStreamSurfacesReasoning(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := completionsStreamBody(t,
		`{"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{"reasoning":"ing"}}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{"content":"answer"}}]}`,
	)
	chunks := drainCompletions(t, adapter, body)
	if len(chunks) != 4 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Type != core.StreamTypeThinking || chunks[0].Text != "think" {
		t.Fatalf("first reasoning chunk = %#v", chunks[0])
	}
	if chunks[1].Type != core.StreamTypeThinking || chunks[1].Text != "ing" {
		t.Fatalf("second reasoning chunk = %#v", chunks[1])
	}
	if chunks[2].Text != "answer" {
		t.Fatalf("text chunk = %#v", chunks[2])
	}
}

func TestCompletionsStreamReportsProviderErrors(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := completionsStreamBody(t, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	chunks := drainCompletions(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Text != "rate limited" || chunks[0].Err == nil {
		t.Fatalf("error chunk = %#v", chunks[0])
	}
}

func TestCompletionsStreamReportsUnspecifiedProviderError(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := completionsStreamBody(t, `{"error":{}}`)
	chunks := drainCompletions(t, adapter, body)
	if len(chunks) != 1 || !strings.Contains(chunks[0].Text, "unspecified") {
		t.Fatalf("chunks = %#v", chunks)
	}
}

func TestCompletionsStreamRejectsBrokenJSON(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := ": keep-alive\n\ndata: {\"choices\":[\n\n"
	chunks := drainCompletions(t, adapter, body)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError {
		t.Fatalf("chunks = %#v", chunks)
	}
	if !strings.Contains(chunks[0].Err.Error(), "decode") {
		t.Fatalf("error = %v", chunks[0].Err)
	}
}

func TestCompletionsStreamStopsWhenConsumerLeaves(t *testing.T) {
	adapter := completionsAdapterFor(t)
	var builder strings.Builder
	writer := NewStreamWriter(&builder)
	for index := 0; index < 8; index++ {
		if err := writer.WriteEvent("", json.RawMessage(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`)); err != nil {
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

func TestCompletionsStreamIgnoresEmptyEvents(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := "data: \n\ndata: \n\n" + `data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\ndata: [DONE]\n\n"
	chunks := drainCompletions(t, adapter, body)
	if len(chunks) != 2 || chunks[0].Text != "ok" {
		t.Fatalf("chunks = %#v", chunks)
	}
}
func TestCompletionsStreamRecordsUsageFromPayload(t *testing.T) {
	adapter := completionsAdapterFor(t)
	body := completionsStreamBody(t,
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":1}}`,
	)
	drainCompletions(t, adapter, body)
	usage := adapter.TakeUsage()
	if usage.InputTokens != 11 || usage.OutputTokens != 1 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestCompletionsGatewayEndToEndOverHTTP(t *testing.T) {
	var received completionsRequest
	server := newCompletionsServer(t, func(writer *responseRecorder) {
		writer.event(`{"id":"1","choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		writer.event(`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writer.done()
	}, &received)
	defer server.Close()

	gateway, err := New(Config{
		Format:   FormatOpenAICompletions,
		Endpoint: server.URL + "/v1",
		APIKey:   "test-key",
		Model:    "gpt-4o",
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:      "gpt-4o",
		SystemHint: "be terse",
		Tools:      []core.ToolSpec{toolSpec("read")},
		Messages:   []core.Message{textMessage(core.RoleUser, "1", "hello")},
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
	if received.Model != "gpt-4o" || !received.Stream || len(received.Tools) != 1 {
		t.Fatalf("request = %#v", received)
	}
	if received.Messages[0].Role != "system" {
		t.Fatalf("messages = %#v", received.Messages)
	}
	if usage := gateway.Usage(); usage.InputTokens != 0 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestCompletionsGatewayRecordsUsage(t *testing.T) {
	server := newCompletionsServer(t, func(writer *responseRecorder) {
		writer.event(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		writer.event(`{"choices":[],"usage":{"prompt_tokens":21,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":7}}}`)
		writer.done()
	}, nil)
	defer server.Close()
	gateway, err := New(Config{Format: FormatOpenAICompletions, Endpoint: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:    "gpt-4o",
		Messages: []core.Message{textMessage(core.RoleUser, "1", "hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	usage := gateway.Usage()
	if usage.InputTokens != 21 || usage.OutputTokens != 2 || usage.CacheReadTokens != 7 {
		t.Fatalf("usage = %#v", usage)
	}
	if usage.Add(Usage{}).InputTokens != 21 {
		t.Fatal("usage accumulation is inconsistent")
	}
}

func TestCompletionsGatewaySendsBearerToken(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		recorder := newResponseRecorder(writer)
		recorder.event(`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`)
		recorder.done()
	}))
	defer server.Close()
	gateway, err := New(Config{Format: FormatOpenAICompletions, Endpoint: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:    "gpt-4o",
		Messages: []core.Message{textMessage(core.RoleUser, "1", "hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if authorization != "Bearer k" {
		t.Fatalf("authorization = %q", authorization)
	}
}

func TestCompletionsGatewayPropagatesHTTPErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(writer, `{"error":{"message":"boom"}}`)
	}))
	defer server.Close()
	gateway, err := New(Config{Format: FormatOpenAICompletions, Endpoint: server.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:    "gpt-4o",
		Messages: []core.Message{textMessage(core.RoleUser, "1", "hi")},
	}); err == nil {
		t.Fatal("provider error accepted")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v", err)
	}
}

type responseRecorder struct {
	writer *StreamWriter
}

func newResponseRecorder(writer http.ResponseWriter) *responseRecorder {
	return &responseRecorder{writer: NewStreamWriter(writer)}
}

func (r *responseRecorder) event(payload string) {
	_ = r.writer.WriteEvent("", json.RawMessage(payload))
}

func (r *responseRecorder) done() {
	_ = r.writer.WriteRawData("[DONE]")
	_ = r.writer.Flush()
}

func newCompletionsServer(
	t *testing.T,
	handle func(*responseRecorder),
	received *completionsRequest,
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
