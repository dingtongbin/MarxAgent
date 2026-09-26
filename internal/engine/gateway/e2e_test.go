// SPDX-License-Identifier: Apache-2.0

package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/gateway"
	"github.com/dingtongbin/MarxAgent/internal/engine/tools"
)

// endToEnd walks the whole L1 loop against a provider that speaks each of the
// three wire formats: user input, a tool call, the tool result coming back, and
// the final answer. Nothing is stubbed below the HTTP boundary.
func endToEnd(t *testing.T, format gateway.APIFormat, corpus func(string) string) {
	t.Helper()
	workspace := t.TempDir()
	pool, err := tools.NewWorkspace(workspace, tools.WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server := newFormatServer(t, format, corpus)
	defer server.Close()

	provider, err := gateway.New(gateway.Config{
		Format:   format,
		Endpoint: server.URL,
		APIKey:   "test-key",
		Model:    "test-model",
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()

	agent, err := core.NewAgent(provider, core.Config{
		Model:         "test-model",
		SessionID:     "session_e2e",
		AgentID:       "agent_e2e",
		SystemHint:    "You are a test agent.",
		MaxIterations: 4,
		Tools:         pool.BuiltinTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("needle in a file"), 0o600); err != nil {
		t.Fatal(err)
	}

	events, err := agent.Run(context.Background(), core.Input{
		Type:    core.InputTypeText,
		Content: "read note.txt and tell me what it contains",
	})
	if err != nil {
		t.Fatal(err)
	}

	var final core.Message
	toolCalls := 0
	toolResults := 0
	partials := 0
	for event := range events {
		switch event.Type {
		case core.EventTypePartial:
			partials++
		case core.EventTypeToolCall:
			var payload core.ToolCallPayload
			if err := json.Unmarshal(event.DataBytes(), &payload); err != nil {
				t.Fatal(err)
			}
			toolCalls += len(payload.Calls)
		case core.EventTypeToolResult:
			var payload core.ToolResultPayload
			if err := json.Unmarshal(event.DataBytes(), &payload); err != nil {
				t.Fatal(err)
			}
			toolResults += len(payload.Results)
		case core.EventTypeError:
			t.Fatalf("agent error: %s", event.Error)
		case core.EventTypeDone:
			var payload core.DonePayload
			if err := json.Unmarshal(event.DataBytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.State.Messages) == 0 {
				t.Fatal("the final state carries no messages")
			}
			final = payload.State.Messages[len(payload.State.Messages)-1]
		}
	}
	if toolCalls == 0 {
		t.Fatal("the loop never reached a tool call")
	}
	if toolResults != toolCalls {
		t.Fatalf("tool calls = %d results = %d", toolCalls, toolResults)
	}
	if partials == 0 {
		t.Fatal("no partial events were published")
	}
	if final.Role != core.RoleAssistant {
		t.Fatalf("final message = %#v", final)
	}
	if strings.TrimSpace(messageText(final)) == "" {
		t.Fatalf("final message is empty: %#v", final)
	}
	if server.Calls() < 2 {
		t.Fatalf("provider calls = %d, want at least 2", server.Calls())
	}
}

// formatServer replays a recorded corpus for the first call and a plain text
// answer for the follow-up, which is the shape of a real tool-using turn.
type formatServer struct {
	*httptest.Server
	mu    sync.Mutex
	calls int
}

func (s *formatServer) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// newFormatServer builds a provider double for one wire format.
func newFormatServer(t *testing.T, format gateway.APIFormat, corpus func(string) string) *formatServer {
	t.Helper()
	server := &formatServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		server.mu.Lock()
		server.calls++
		stage := "answer"
		if server.calls == 1 {
			stage = "read"
		}
		server.mu.Unlock()
		if stage == "read" {
			verifyFormatRequest(t, format, request)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, corpus(stage))
	}))
	return server
}

func verifyFormatRequest(t *testing.T, format gateway.APIFormat, request *http.Request) {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Errorf("read body: %v", err)
		return
	}
	var payload struct {
		Model  string            `json:"model"`
		Stream bool              `json:"stream"`
		Tools  []json.RawMessage `json:"tools"`
		System json.RawMessage   `json:"system"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Errorf("decode %s body: %v", format, err)
		return
	}
	if payload.Model != "test-model" || !payload.Stream {
		t.Errorf("%s request = %s", format, body)
	}
	if len(payload.Tools) == 0 {
		t.Errorf("%s request carried no tools: %s", format, body)
	}
	switch format {
	case gateway.FormatAnthropicMessages:
		if !strings.Contains(string(payload.System), "test agent") {
			t.Errorf("anthropic system = %s", payload.System)
		}
		if request.Header.Get("x-api-key") == "" {
			t.Errorf("anthropic key header is missing")
		}
	default:
		if request.Header.Get("Authorization") == "" {
			t.Errorf("%s authorization header is missing", format)
		}
	}
}

func messageText(message core.Message) string {
	var builder strings.Builder
	for _, block := range message.Content {
		if block.Type == core.ContentTypeText {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

func corpusFor(format gateway.APIFormat) func(string) string {
	return func(stage string) string {
		switch format {
		case gateway.FormatOpenAICompletions:
			return completionsCorpus(stage)
		case gateway.FormatAnthropicMessages:
			return anthropicCorpus(stage)
		default:
			return responsesCorpus(stage)
		}
	}
}

func completionsCorpus(stage string) string {
	if stage == "answer" {
		return strings.Join([]string{
			`data: {"id":"1","choices":[{"index":0,"delta":{"content":"The file contains a needle."}}]}`,
			"",
			`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			"",
			`data: [DONE]`,
			"",
		}, "\n")
	}
	return strings.Join([]string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"note.txt\"}"}}]}}]}`,
		"",
		`data: {"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")
}

func anthropicCorpus(stage string) string {
	if stage == "answer" {
		return strings.Join([]string{
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"",
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The file contains a needle."}}`,
			"",
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			"",
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			"",
		}, "\n")
	}
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10}}}`,
		"",
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read","input":{}}}`,
		"",
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"note.txt\"}"}}`,
		"",
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		"",
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
}

func responsesCorpus(stage string) string {
	if stage == "answer" {
		return strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"The file contains a needle."}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_2"}}`,
			"",
		}, "\n")
	}
	return strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		"",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":""}}`,
		"",
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"path\":\"note.txt\"}"}`,
		"",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"note.txt\"}"}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_1"}}`,
		"",
	}, "\n")
}

func TestEndToEndAllFormats(t *testing.T) {
	for _, format := range []gateway.APIFormat{
		gateway.FormatOpenAICompletions,
		gateway.FormatAnthropicMessages,
		gateway.FormatOpenAIResponses,
	} {
		t.Run(string(format), func(t *testing.T) {
			endToEnd(t, format, corpusFor(format))
		})
	}
}

func TestGatewayRejectsAnUnreachableEndpoint(t *testing.T) {
	provider, err := gateway.New(gateway.Config{
		Format:   gateway.FormatOpenAICompletions,
		Endpoint: "http://127.0.0.1:1",
		APIKey:   "k",
		Model:    "test-model",
		Timeout:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := core.NewAgent(provider, core.Config{
		Model:         "test-model",
		SessionID:     "session_down",
		AgentID:       "agent_down",
		MaxIterations: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := agent.Run(context.Background(), core.Input{
		Type:    core.InputTypeText,
		Content: "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	var failures int
	for event := range events {
		if event.Type == core.EventTypeError {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("an unreachable provider produced no error event")
	}
	provider.Close()
}

func TestGatewayRefusesAnUnusableConfiguration(t *testing.T) {
	cases := []gateway.Config{
		{Format: "nope", Endpoint: "http://127.0.0.1:1"},
		{Format: gateway.FormatOpenAICompletions},
		{Format: gateway.FormatOpenAICompletions, Endpoint: "http://127.0.0.1:1", MaxRetries: -1},
		{Format: gateway.FormatAnthropicMessages, Endpoint: "http://127.0.0.1:1", Thinking: &gateway.ThinkingConfig{Enabled: true, BudgetTokens: -1}},
	}
	for _, config := range cases {
		if _, err := gateway.New(config); err == nil {
			t.Fatalf("configuration was accepted: %#v", config)
		}
	}
}

func TestEndToEndRejectsAnInvalidRequest(t *testing.T) {
	provider, err := gateway.New(gateway.Config{
		Format:   gateway.FormatOpenAICompletions,
		Endpoint: "http://127.0.0.1:1",
		APIKey:   "k",
		Model:    "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ChatStream(context.Background(), core.ChatRequest{
		Model:    "test-model",
		Messages: []core.Message{{Role: core.RoleUser}},
	}); err == nil {
		t.Fatal("a request with empty content was accepted")
	} else if !errors.Is(err, gateway.ErrInvalidRequest) {
		t.Fatalf("error = %v", err)
	}
	provider.Close()
}
