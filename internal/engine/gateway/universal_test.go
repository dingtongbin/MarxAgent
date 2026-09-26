// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func textMessage(role core.Role, id, text string) core.Message {
	return core.Message{
		ID:        id,
		Role:      role,
		Content:   []core.ContentBlock{{Type: core.ContentTypeText, Text: text}},
		CreatedAt: time.Unix(0, 0).UTC(),
	}
}

func toolUseMessage(id string, calls ...core.ContentBlock) core.Message {
	message := core.Message{ID: id, Role: core.RoleAssistant, CreatedAt: time.Unix(0, 0).UTC()}
	for index, call := range calls {
		call.ToolCallID = "call_" + string(rune('a'+index))
		if call.Input == nil {
			call.Input = json.RawMessage(`{}`)
		}
		message.Content = append(message.Content, call)
	}
	return message
}

func TestHoistSystemMessages(t *testing.T) {
	cases := []struct {
		name        string
		messages    []core.Message
		wantSystem  string
		wantHistory int
	}{
		{
			name:        "no messages",
			messages:    nil,
			wantSystem:  "",
			wantHistory: 0,
		},
		{
			name:        "only conversation",
			messages:    []core.Message{textMessage(core.RoleUser, "1", "hi")},
			wantSystem:  "",
			wantHistory: 1,
		},
		{
			name: "system messages are joined in order",
			messages: []core.Message{
				textMessage(core.RoleSystem, "1", "first"),
				textMessage(core.RoleUser, "2", "hi"),
				textMessage(core.RoleSystem, "3", "second"),
			},
			wantSystem:  "first\n\nsecond",
			wantHistory: 1,
		},
		{
			name:        "empty system message is dropped",
			messages:    []core.Message{textMessage(core.RoleSystem, "1", "   "), textMessage(core.RoleUser, "2", "hi")},
			wantSystem:  "",
			wantHistory: 1,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			system, history := HoistSystemMessages(test.messages)
			if system != test.wantSystem || len(history) != test.wantHistory {
				t.Fatalf("system = %q history = %d, want %q and %d", system, len(history), test.wantSystem, test.wantHistory)
			}
		})
	}
}

func TestSystemTextPrefersHintThenHoisted(t *testing.T) {
	request := &core.ChatRequest{
		SystemHint: "hint",
		Messages:   []core.Message{textMessage(core.RoleSystem, "1", "hoisted")},
	}
	if got := SystemText(request); got != "hint\n\nhoisted" {
		t.Fatalf("system text = %q", got)
	}
	if got := SystemText(&core.ChatRequest{}); got != "" {
		t.Fatalf("empty system text = %q", got)
	}
	if got := SystemText(nil); got != "" {
		t.Fatalf("nil system text = %q", got)
	}
	if got := ConversationMessages(nil); got != nil {
		t.Fatalf("nil conversation = %#v", got)
	}
}

func TestValidateRequestAccepts(t *testing.T) {
	request := &core.ChatRequest{
		Model:       "claude-sonnet-4-5",
		MaxTokens:   1024,
		Temperature: 0.7,
		SystemHint:  "be brief",
		Tools: []core.ToolSpec{{
			Name:        "read",
			Description: "Read a file.",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
		Messages: []core.Message{
			textMessage(core.RoleUser, "1", "read the file"),
			{
				ID:   "2",
				Role: core.RoleAssistant,
				Content: []core.ContentBlock{
					{Type: core.ContentTypeThinking, Thinking: "considering", ThinkingSig: "sig"},
					{Type: core.ContentTypeToolUse, ToolCallID: "call_1", ToolName: "read", Input: json.RawMessage(`{"path":"a"}`)},
				},
			},
			{
				ID:   "3",
				Role: core.RoleTool,
				Content: []core.ContentBlock{{
					Type:       core.ContentTypeToolResult,
					ToolCallID: "call_1",
					ToolName:   "read",
					Output:     json.RawMessage(`"contents"`),
				}},
			},
			{
				ID:   "4",
				Role: core.RoleUser,
				Content: []core.ContentBlock{{
					Type:        core.ContentTypeImage,
					ImageData:   "aGVsbG8=",
					MediaFormat: "png",
				}},
			},
			{
				ID:   "5",
				Role: core.RoleAssistant,
				Content: []core.ContentBlock{
					{Type: core.ContentTypeReasoning, EncryptedContent: "opaque"},
					{Type: core.ContentTypeText, Text: "done"},
				},
			},
			textMessage(core.RoleUser, "6", "thanks"),
		},
	}
	if err := ValidateRequest(request); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestValidateRequestRejects(t *testing.T) {
	valid := func() *core.ChatRequest {
		return &core.ChatRequest{
			Model:    "model",
			Messages: []core.Message{textMessage(core.RoleUser, "1", "hi")},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*core.ChatRequest)
		wantErr bool
	}{
		{name: "nil request"},
		{name: "empty model", mutate: func(r *core.ChatRequest) { r.Model = "  " }},
		{name: "negative max tokens", mutate: func(r *core.ChatRequest) { r.MaxTokens = -1 }},
		{name: "temperature above range", mutate: func(r *core.ChatRequest) { r.Temperature = 2.5 }},
		{name: "negative temperature", mutate: func(r *core.ChatRequest) { r.Temperature = -0.5 }},
		{
			name: "tool without name",
			mutate: func(r *core.ChatRequest) {
				r.Tools = []core.ToolSpec{{Parameters: json.RawMessage(`{}`)}}
			},
		},
		{
			name: "tool schema is not an object",
			mutate: func(r *core.ChatRequest) {
				r.Tools = []core.ToolSpec{{Name: "read", Description: "d", Parameters: json.RawMessage(`[]`)}}
			},
		},
		{
			name: "duplicate tool",
			mutate: func(r *core.ChatRequest) {
				r.Tools = []core.ToolSpec{
					{Name: "read", Description: "d", Parameters: json.RawMessage(`{}`)},
					{Name: "read", Description: "d", Parameters: json.RawMessage(`{}`)},
				}
			},
		},
		{name: "unsupported role", mutate: func(r *core.ChatRequest) { r.Messages[0].Role = core.Role("moderator") }},
		{name: "empty content", mutate: func(r *core.ChatRequest) { r.Messages[0].Content = nil }},
		{
			name:   "unknown block type",
			mutate: func(r *core.ChatRequest) { r.Messages[0].Content = []core.ContentBlock{{Type: "video"}} },
		},
		{
			name: "image without data",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeImage, MediaFormat: "png"}}
			},
		},
		{
			name: "image without media type",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeImage, ImageData: "aGk="}}
			},
		},
		{
			name: "image with unknown media type",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeImage, ImageData: "aGk=", MediaFormat: "image/tiff"}}
			},
		},
		{
			name: "empty thinking",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeThinking}}
			},
		},
		{
			name: "empty reasoning",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeReasoning}}
			},
		},
		{
			name: "tool call without id",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeToolUse, ToolName: "read", Input: json.RawMessage(`{}`)}}
			},
		},
		{
			name: "tool call with array input",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeToolUse, ToolCallID: "c", ToolName: "read", Input: json.RawMessage(`[]`)}}
			},
		},
		{
			name: "tool call names an undeclared tool",
			mutate: func(r *core.ChatRequest) {
				r.Tools = []core.ToolSpec{{Name: "read", Description: "d", Parameters: json.RawMessage(`{}`)}}
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeToolUse, ToolCallID: "c", ToolName: "write", Input: json.RawMessage(`{}`)}}
			},
		},
		{
			name: "tool message without a result",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Role = core.RoleTool
			},
		},
		{
			name: "tool result without id",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Role = core.RoleTool
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeToolResult, Output: json.RawMessage(`"x"`)}}
			},
		},
		{
			name: "tool result without output",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Role = core.RoleTool
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeToolResult, ToolCallID: "c"}}
			},
		},
		{
			name: "tool result with invalid json",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Role = core.RoleTool
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeToolResult, ToolCallID: "c", Output: json.RawMessage(`{`)}}
			},
		},
		{
			name: "unverified tool call is allowed without declarations",
			mutate: func(r *core.ChatRequest) {
				r.Messages[0].Content = []core.ContentBlock{{Type: core.ContentTypeToolUse, ToolCallID: "c", ToolName: "read", Input: json.RawMessage(`{}`)}}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var request *core.ChatRequest
			if test.mutate != nil {
				request = valid()
				test.mutate(request)
			}
			err := ValidateRequest(request)
			if test.name == "nil request" {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("nil request error = %v", err)
				}
				return
			}
			if test.name == "unverified tool call is allowed without declarations" {
				if err != nil {
					t.Fatalf("undeclared tool rejected without declarations: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestNormalizeMediaType(t *testing.T) {
	cases := map[string]string{
		"png":         "image/png",
		"PNG":         "image/png",
		"image/png":   "image/png",
		" jpg":        "image/jpeg",
		"jpeg":        "image/jpeg",
		"image/jpeg":  "image/jpeg",
		"gif":         "image/gif",
		"webp":        "image/webp",
		"":            "",
		"image/tiff":  "",
		"application": "",
	}
	for input, want := range cases {
		if got := NormalizeMediaType(input); got != want {
			t.Fatalf("NormalizeMediaType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestResponseChainHelpers(t *testing.T) {
	first := textMessage(core.RoleAssistant, "1", "one")
	first.Metadata = map[string]any{"response_id": "resp_1"}
	second := textMessage(core.RoleAssistant, "2", "two")
	second.Metadata = map[string]any{"response_id": "resp_2"}
	history := []core.Message{textMessage(core.RoleUser, "0", "hi"), first, textMessage(core.RoleUser, "3", "next"), second}
	if got := LastResponseID(history); got != "resp_2" {
		t.Fatalf("last response id = %q", got)
	}
	if got := MessageResponseID(first); got != "resp_1" {
		t.Fatalf("message response id = %q", got)
	}
	if got := MessageResponseID(textMessage(core.RoleUser, "4", "x")); got != "" {
		t.Fatalf("user response id = %q", got)
	}
	trimmed, found := TrimChainHistory(history, "resp_1")
	if !found || len(trimmed) != 2 || trimmed[0].ID != "3" {
		t.Fatalf("trimmed = %#v found = %v", trimmed, found)
	}
	if _, found := TrimChainHistory(history, "resp_missing"); found {
		t.Fatal("unknown chain was reported as resumable")
	}
	if _, found := TrimChainHistory(history, ""); found {
		t.Fatal("empty chain was reported as resumable")
	}
}

func TestBlockHelpers(t *testing.T) {
	message := core.Message{
		Role: core.RoleUser,
		Content: []core.ContentBlock{
			{Type: core.ContentTypeText, Text: "hello "},
			{Type: core.ContentTypeImage, ImageData: "aGk="},
			{Type: core.ContentTypeText, Text: "world"},
			{Type: core.ContentTypeToolResult, ToolCallID: "c", Output: json.RawMessage(`1`)},
		},
	}
	if got := MessageText(message); got != "hello world" {
		t.Fatalf("message text = %q", got)
	}
	results := ToolResultBlocks(message)
	if len(results) != 1 || results[0].ToolCallID != "c" {
		t.Fatalf("tool results = %#v", results)
	}
	if got := ToolResultBlocks(core.Message{}); len(got) != 0 {
		t.Fatalf("empty results = %#v", got)
	}
}

func TestJSONHelpers(t *testing.T) {
	if !IsJSONObject(json.RawMessage(`{"a":1}`)) {
		t.Fatal("object rejected")
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`[]`), json.RawMessage(`null`), json.RawMessage(`{`)} {
		if IsJSONObject(raw) {
			t.Fatalf("%q accepted as an object", raw)
		}
	}
	original := json.RawMessage(`{"a":1}`)
	clone := CloneRawJSON(original)
	clone[2] = 'z'
	if string(original) != `{"a":1}` {
		t.Fatal("CloneRawJSON aliased its source")
	}
	if CloneRawJSON(nil) != nil {
		t.Fatal("nil clone is not nil")
	}
	messages := []core.Message{{
		ID:      "1",
		Content: []core.ContentBlock{{Type: core.ContentTypeToolUse, ToolCallID: "c", Input: json.RawMessage(`{"p":1}`)}},
	}}
	cloned := CloneMessages(messages)
	cloned[0].Content[0].Input[5] = 'z'
	if string(messages[0].Content[0].Input) != `{"p":1}` {
		t.Fatal("CloneMessages aliased block input")
	}
	if CloneMessages(nil) != nil || CloneBlocks(nil) != nil || CloneTools(nil) != nil {
		t.Fatal("nil clones are not nil")
	}
	tools := []core.ToolSpec{{Name: "read", Parameters: json.RawMessage(`{"type":"object"}`)}}
	clonedTools := CloneTools(tools)
	clonedTools[0].Parameters[2] = 'z'
	if string(tools[0].Parameters) != `{"type":"object"}` {
		t.Fatal("CloneTools aliased parameters")
	}
	clonedBlocks := CloneBlocks([]core.ContentBlock{{Output: json.RawMessage(`"x"`)}})
	clonedBlocks[0].Output[1] = 'z'
	if string(messages[0].Content[0].Input) == "" {
		t.Fatal("unexpected clone state")
	}
}

func TestCacheHintsFor(t *testing.T) {
	if got := CacheHintsFor(nil); got.StablePrefix || got.HistoryPrefix || got.MaxBreakpoints != DefaultCacheBreakpoints {
		t.Fatalf("nil hints = %#v", got)
	}
	bare := CacheHintsFor(&core.ChatRequest{Messages: []core.Message{textMessage(core.RoleUser, "1", "hi")}})
	if bare.StablePrefix || bare.HistoryPrefix {
		t.Fatalf("bare hints = %#v", bare)
	}
	rich := CacheHintsFor(&core.ChatRequest{
		SystemHint: "hint",
		Tools:      []core.ToolSpec{{Name: "read", Description: "d", Parameters: json.RawMessage(`{}`)}},
		Messages:   []core.Message{textMessage(core.RoleUser, "1", "hi"), textMessage(core.RoleAssistant, "2", "yo")},
	})
	if !rich.StablePrefix || !rich.HistoryPrefix {
		t.Fatalf("rich hints = %#v", rich)
	}
}

func TestUsageAccumulation(t *testing.T) {
	total := Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4}.
		Add(Usage{InputTokens: 5, OutputTokens: 1, CacheReadTokens: 1})
	if total.InputTokens != 15 || total.OutputTokens != 3 || total.CacheReadTokens != 4 || total.CacheWriteTokens != 4 {
		t.Fatalf("usage = %#v", total)
	}
	stats := CacheStats{ReadTokens: 5, InputTokens: 10}
	if stats.HitRate() != 0.5 {
		t.Fatalf("hit rate = %v", stats.HitRate())
	}
	if (CacheStats{}).HitRate() != 0 {
		t.Fatal("empty hit rate is not zero")
	}
}
