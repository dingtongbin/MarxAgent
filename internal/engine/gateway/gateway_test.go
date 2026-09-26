// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

const testFormat APIFormat = "test-format"

type testAdapter struct {
	mu             sync.Mutex
	format         APIFormat
	usage          Usage
	buildErr       error
	thinkingErr    error
	chainID        string
	chainResumable bool
	recorded       []string
	invalidations  []string
	responseID     string
	appliedHints   CacheHints
	requests       []core.ChatRequest
}

func (a *testAdapter) Format() APIFormat {
	if a.format == "" {
		return testFormat
	}
	return a.format
}

func (a *testAdapter) BuildRequest(request *core.ChatRequest) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.buildErr != nil {
		return nil, a.buildErr
	}
	a.requests = append(a.requests, *request)
	return map[string]any{"model": request.Model, "messages": len(request.Messages)}, nil
}

func (a *testAdapter) ParseStream(body io.Reader) iter.Seq[core.StreamChunk] {
	return func(yield func(core.StreamChunk) bool) {
		for event := range DecodeStream(body) {
			switch event.Name {
			case "error":
				yield(core.StreamChunk{Type: core.StreamTypeError, Err: errors.New(event.Data)})
				return
			case "usage":
				continue
			case "response":
				if !yield(core.StreamChunk{Type: core.StreamTypeText, Text: eventText(event.Data), ResponseID: a.responseID}) {
					return
				}
			default:
				if !yield(core.StreamChunk{Type: core.StreamTypeText, Text: eventText(event.Data)}) {
					return
				}
			}
		}
		yield(core.StreamChunk{Type: core.StreamTypeDone})
	}
}

func eventText(data string) string {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil || payload.Text == "" {
		return data
	}
	return payload.Text
}

func (a *testAdapter) NormalizeUsage(map[string]any) Usage { return Usage{} }

func (a *testAdapter) TakeUsage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

func (a *testAdapter) ApplyThinking(any, ThinkingConfig) error { return a.thinkingErr }

func (a *testAdapter) ApplyCacheHints(_ any, hints CacheHints) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appliedHints = hints
	return nil
}

func (a *testAdapter) ExtractCacheStats(usage Usage) CacheStats {
	return identityCacheHintMapper{}.ExtractCacheStats(usage)
}

func (a *testAdapter) ChainUsage(*core.ChatRequest) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.chainID, a.chainResumable
}

func (a *testAdapter) RecordChain(responseID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recorded = append(a.recorded, responseID)
}

func (a *testAdapter) InvalidateChain(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.invalidations = append(a.invalidations, reason)
}

func newTestGateway(t *testing.T, adapter APIAdapter, mutate func(*Config)) *ProviderGateway {
	t.Helper()
	config := Config{
		Format:   testFormat,
		Endpoint: "http://127.0.0.1:1/unused",
		APIKey:   "secret",
		Model:    "test-model",
		Timeout:  10 * time.Second,
	}
	if mutate != nil {
		mutate(&config)
	}
	gateway, err := NewWithAdapter(config, adapter)
	if err != nil {
		t.Fatal(err)
	}
	return gateway
}

func simpleRequest() core.ChatRequest {
	return core.ChatRequest{
		Model:    "test-model",
		Messages: []core.Message{textMessage(core.RoleUser, "1", "hi")},
	}
}

func collectChunks(t *testing.T, stream <-chan core.StreamChunk) []core.StreamChunk {
	t.Helper()
	chunks := make([]core.StreamChunk, 0, 8)
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func TestNewWithAdapterRejectsInvalidConfiguration(t *testing.T) {
	base := Config{Format: testFormat, Endpoint: "http://127.0.0.1:1"}
	cases := []struct {
		name    string
		config  Config
		adapter APIAdapter
	}{
		{name: "nil adapter", config: base, adapter: nil},
		{name: "empty endpoint", config: Config{Format: testFormat}, adapter: &testAdapter{}},
		{name: "empty format", config: Config{Endpoint: "http://127.0.0.1:1"}, adapter: &testAdapter{}},
		{name: "negative retries", config: withMutations(base, func(c *Config) { c.MaxRetries = -1 }), adapter: &testAdapter{}},
		{name: "negative thinking budget", config: withMutations(base, func(c *Config) {
			c.Thinking = &ThinkingConfig{Enabled: true, BudgetTokens: -1}
		}), adapter: &testAdapter{}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWithAdapter(test.config, test.adapter); !errors.Is(err, ErrInvalidGateway) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func withMutations(config Config, mutate func(*Config)) Config {
	mutate(&config)
	return config
}

func TestNewRejectsUnregisteredFormat(t *testing.T) {
	if _, err := New(Config{Format: "nope", Endpoint: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("unregistered format accepted")
	} else {
		var unsupported *ErrUnsupportedFormat
		if !errors.As(err, &unsupported) {
			t.Fatalf("error = %v, want ErrUnsupportedFormat", err)
		}
	}
	for _, format := range Formats() {
		if format == "nope" {
			t.Fatal("unknown format is registered")
		}
	}
	if err := registerAdapter("", nil); err == nil {
		t.Fatal("empty format registered")
	}
	if err := registerAdapter("x", nil); err == nil {
		t.Fatal("nil factory registered")
	}
	if err := registerAdapter("x", func() APIAdapter { return &testAdapter{} }); err != nil {
		t.Fatal(err)
	}
	if err := registerAdapter("x", func() APIAdapter { return &testAdapter{} }); err == nil {
		t.Fatal("duplicate registration accepted")
	}
	if _, err := NewAdapter("x"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAdapter("missing"); err == nil {
		t.Fatal("missing adapter returned")
	}
}

func TestResolveEndpoint(t *testing.T) {
	cases := []struct {
		format   APIFormat
		endpoint string
		want     string
	}{
		{format: FormatOpenAICompletions, endpoint: "https://api.example.com/v1/", want: "https://api.example.com/v1/chat/completions"},
		{format: FormatAnthropicMessages, endpoint: "https://api.anthropic.com/v1", want: "https://api.anthropic.com/v1/messages"},
		{format: FormatOpenAIResponses, endpoint: "api.openai.com/v1", want: "https://api.openai.com/v1/responses"},
		{format: testFormat, endpoint: "http://127.0.0.1:8080/base/", want: "http://127.0.0.1:8080/base"},
	}
	for _, test := range cases {
		got, err := resolveEndpoint(Config{Format: test.format, Endpoint: test.endpoint})
		if err != nil || got != test.want {
			t.Fatalf("resolveEndpoint(%q, %q) = %q err = %v, want %q", test.format, test.endpoint, got, err, test.want)
		}
	}
	if _, err := resolveEndpoint(Config{Endpoint: "https://api.example.com"}); err == nil {
		t.Fatal("empty format resolved")
	}
	if _, err := resolveEndpoint(Config{Format: testFormat}); err == nil {
		t.Fatal("empty endpoint resolved")
	}
}

func TestGatewayNameAndFormat(t *testing.T) {
	gateway := newTestGateway(t, &testAdapter{}, nil)
	if gateway.Format() != testFormat {
		t.Fatalf("format = %q", gateway.Format())
	}
	if gateway.Name() != "gateway:test-format:test-model" {
		t.Fatalf("name = %q", gateway.Name())
	}
	var nilGateway *ProviderGateway
	if nilGateway.Format() != "" || nilGateway.Name() != "" || nilGateway.Usage() != (Usage{}) {
		t.Fatal("nil gateway returned state")
	}
	nilGateway.ResetUsage()
	nilGateway.InvalidateChain("x")
	nilGateway.Close()
}

func TestChatStreamRejectsBadInput(t *testing.T) {
	gateway := newTestGateway(t, &testAdapter{}, nil)
	var nilGateway *ProviderGateway
	if _, err := nilGateway.ChatStream(context.Background(), simpleRequest()); err == nil {
		t.Fatal("nil gateway accepted a request")
	}
	if _, err := gateway.ChatStream(nil, simpleRequest()); !errors.Is(err, ErrInvalidGateway) {
		t.Fatalf("nil context error = %v", err)
	}
	invalid := simpleRequest()
	invalid.Model = ""
	if _, err := gateway.ChatStream(context.Background(), invalid); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid request error = %v", err)
	}
}

func TestChatStreamPropagatesBuildFailures(t *testing.T) {
	buildErr := errors.New("cannot translate")
	gateway := newTestGateway(t, &testAdapter{buildErr: buildErr}, nil)
	if _, err := gateway.ChatStream(context.Background(), simpleRequest()); !errors.Is(err, buildErr) {
		t.Fatalf("build error = %v", err)
	}
	thinkingErr := errors.New("cannot enable thinking")
	gateway = newTestGateway(t, &testAdapter{thinkingErr: thinkingErr}, func(config *Config) {
		config.Thinking = &ThinkingConfig{Enabled: true, BudgetTokens: 1024}
	})
	if _, err := gateway.ChatStream(context.Background(), simpleRequest()); !errors.Is(err, thinkingErr) {
		t.Fatalf("thinking error = %v", err)
	}
}

func TestChatStreamStreamsAndRecordsUsage(t *testing.T) {
	var received map[string]any
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		headers = request.Header.Clone()
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &received)
		stream := NewStreamWriter(writer)
		_ = stream.WriteComment("open")
		_ = stream.WriteEvent("message", map[string]string{"text": "hello"})
		_ = stream.Flush()
		_ = stream.WriteEvent("message", map[string]string{"text": " world"})
		_ = stream.WriteEvent("response", map[string]string{"text": "!"})
		_ = stream.WriteEvent("usage", map[string]int{"input": 7})
	}))
	defer server.Close()

	adapter := &testAdapter{usage: Usage{InputTokens: 7, OutputTokens: 3, CacheReadTokens: 2}, responseID: "resp_1"}
	gateway := newTestGateway(t, adapter, func(config *Config) {
		config.Endpoint = server.URL
		config.ServerState = true
	})
	stream, err := gateway.ChatStream(context.Background(), core.ChatRequest{
		Model:      "test-model",
		SystemHint: "be brief",
		Tools:      []core.ToolSpec{{Name: "read", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Messages: []core.Message{
			textMessage(core.RoleUser, "1", "hi"),
			textMessage(core.RoleAssistant, "2", "hello"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectChunks(t, stream)
	if len(chunks) != 4 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0].Text != "hello" || chunks[1].Text != " world" || chunks[2].ResponseID != "resp_1" {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[3].Type != core.StreamTypeDone {
		t.Fatalf("terminal chunk = %#v", chunks[3])
	}
	if usage := gateway.Usage(); usage.InputTokens != 7 || usage.OutputTokens != 3 || usage.CacheReadTokens != 2 {
		t.Fatalf("usage = %#v", usage)
	}
	gateway.ResetUsage()
	if gateway.Usage() != (Usage{}) {
		t.Fatal("usage was not reset")
	}
	if headers.Get("Authorization") != "" && headers.Get("x-api-key") != "secret" {
		t.Fatalf("headers = %v", headers)
	}
	if headers.Get("Content-Type") != "application/json" || headers.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers = %v", headers)
	}
	if received["model"] != "test-model" {
		t.Fatalf("payload = %#v", received)
	}
	if !adapter.appliedHints.StablePrefix || !adapter.appliedHints.HistoryPrefix {
		t.Fatalf("hints = %#v", adapter.appliedHints)
	}
	if len(adapter.recorded) != 1 || adapter.recorded[0] != "resp_1" {
		t.Fatalf("recorded = %#v", adapter.recorded)
	}
	gateway.Close()
}

func TestChatStreamReportsProviderErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		stream := NewStreamWriter(writer)
		_ = stream.WriteEvent("error", "model overloaded")
	}))
	defer server.Close()
	gateway := newTestGateway(t, &testAdapter{}, func(config *Config) { config.Endpoint = server.URL })
	stream, err := gateway.ChatStream(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectChunks(t, stream)
	if len(chunks) != 1 || chunks[0].Type != core.StreamTypeError || chunks[0].Err == nil {
		t.Fatalf("chunks = %#v", chunks)
	}
	if !strings.Contains(chunks[0].Err.Error(), "model overloaded") {
		t.Fatalf("error = %v", chunks[0].Err)
	}
}

func TestChatStreamFailsOnHTTPError(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(writer, "slow down")
	}))
	defer server.Close()
	gateway := newTestGateway(t, &testAdapter{}, func(config *Config) {
		config.Endpoint = server.URL
		config.MaxRetries = 2
	})
	if _, err := gateway.ChatStream(context.Background(), simpleRequest()); err == nil {
		t.Fatal("HTTP error accepted")
	} else if !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("error = %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestChatStreamRetriesRecoverableStatus(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		stream := NewStreamWriter(writer)
		_ = stream.WriteEvent("message", map[string]string{"text": "recovered"})
	}))
	defer server.Close()
	gateway := newTestGateway(t, &testAdapter{}, func(config *Config) {
		config.Endpoint = server.URL
		config.MaxRetries = 1
	})
	stream, err := gateway.ChatStream(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	chunks := collectChunks(t, stream)
	if len(chunks) != 2 || chunks[0].Text != "recovered" || chunks[1].Type != core.StreamTypeDone {
		t.Fatalf("chunks = %#v", chunks)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestChatStreamStopsOnCanceledContext(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		stream := NewStreamWriter(writer)
		for index := 0; index < 64; index++ {
			if err := stream.WriteEvent("message", map[string]string{"text": "spam"}); err != nil {
				return
			}
			if index == 0 {
				_ = stream.Flush()
			}
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	gateway := newTestGateway(t, &testAdapter{}, func(config *Config) { config.Endpoint = server.URL })
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := gateway.ChatStream(ctx, simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	<-stream
	cancel()
	collectChunks(t, stream)
}

func TestChatStreamHonorsGatewayTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		stream := NewStreamWriter(writer)
		_ = stream.WriteEvent("message", map[string]string{"text": "one"})
	}))
	defer server.Close()
	gateway := newTestGateway(t, &testAdapter{}, func(config *Config) {
		config.Endpoint = server.URL
		config.Timeout = 50 * time.Millisecond
	})
	stream, err := gateway.ChatStream(context.Background(), simpleRequest())
	if err != nil {
		t.Fatal(err)
	}
	collectChunks(t, stream)
}

func TestResponseChainResumesThenFallsBackToFullHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		stream := NewStreamWriter(writer)
		_ = stream.WriteEvent("response", map[string]string{"text": "ok"})
	}))
	defer server.Close()

	adapter := &testAdapter{chainID: "resp_1", chainResumable: true, responseID: "resp_1"}
	gateway := newTestGateway(t, adapter, func(config *Config) {
		config.Endpoint = server.URL
		config.ServerState = true
	})
	assistant := textMessage(core.RoleAssistant, "1", "one")
	assistant.Metadata = map[string]any{"response_id": "resp_1"}
	history := []core.Message{textMessage(core.RoleUser, "0", "hi"), assistant, textMessage(core.RoleUser, "2", "next")}

	first, err := gateway.ChatStream(context.Background(), core.ChatRequest{Model: "test-model", Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	collectChunks(t, first)
	if len(adapter.requests) != 1 || len(adapter.requests[0].Messages) != 3 {
		t.Fatalf("first request = %#v", adapter.requests)
	}

	second, err := gateway.ChatStream(context.Background(), core.ChatRequest{Model: "test-model", Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	collectChunks(t, second)
	if len(adapter.requests) != 2 {
		t.Fatalf("requests = %d", len(adapter.requests))
	}
	if got := adapter.requests[1].Messages; len(got) != 1 || got[0].ID != "2" {
		t.Fatalf("resumed request messages = %#v", got)
	}

	adapter.chainID = "resp_missing"
	third, err := gateway.ChatStream(context.Background(), core.ChatRequest{Model: "test-model", Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	collectChunks(t, third)
	if got := adapter.requests[2].Messages; len(got) != 3 {
		t.Fatalf("unresumable request messages = %#v", got)
	}
	if len(adapter.invalidations) == 0 {
		t.Fatal("unresumable chain was not invalidated")
	}
}

func TestResponseChainStaysDisabledWithoutServerState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		stream := NewStreamWriter(writer)
		_ = stream.WriteEvent("response", map[string]string{"text": "ok"})
	}))
	defer server.Close()
	adapter := &testAdapter{chainID: "resp_1", chainResumable: true, responseID: "resp_1"}
	gateway := newTestGateway(t, adapter, func(config *Config) { config.Endpoint = server.URL })
	assistant := textMessage(core.RoleAssistant, "1", "one")
	assistant.Metadata = map[string]any{"response_id": "resp_1"}
	history := []core.Message{textMessage(core.RoleUser, "0", "hi"), assistant, textMessage(core.RoleUser, "2", "next")}
	for attempt := 0; attempt < 2; attempt++ {
		stream, err := gateway.ChatStream(context.Background(), core.ChatRequest{Model: "test-model", Messages: history})
		if err != nil {
			t.Fatal(err)
		}
		collectChunks(t, stream)
	}
	for index, request := range adapter.requests {
		if len(request.Messages) != 3 {
			t.Fatalf("request %d trimmed history without server state: %#v", index, request.Messages)
		}
	}
	if len(adapter.invalidations) == 0 {
		t.Fatal("adapter chain was not invalidated")
	}
}

func TestInvalidateChainForcesFullResend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		stream := NewStreamWriter(writer)
		_ = stream.WriteEvent("response", map[string]string{"text": "ok"})
	}))
	defer server.Close()
	adapter := &testAdapter{chainID: "resp_1", chainResumable: true, responseID: "resp_1"}
	gateway := newTestGateway(t, adapter, func(config *Config) {
		config.Endpoint = server.URL
		config.ServerState = true
	})
	assistant := textMessage(core.RoleAssistant, "1", "one")
	assistant.Metadata = map[string]any{"response_id": "resp_1"}
	history := []core.Message{textMessage(core.RoleUser, "0", "hi"), assistant, textMessage(core.RoleUser, "2", "next")}

	first, err := gateway.ChatStream(context.Background(), core.ChatRequest{Model: "test-model", Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	collectChunks(t, first)
	gateway.InvalidateChain("manual")

	second, err := gateway.ChatStream(context.Background(), core.ChatRequest{Model: "test-model", Messages: history})
	if err != nil {
		t.Fatal(err)
	}
	collectChunks(t, second)
	if got := adapter.requests[1].Messages; len(got) != 3 {
		t.Fatalf("history after invalidation = %#v", got)
	}
}

func TestApplyResponseChainRejectsEmptyHistory(t *testing.T) {
	adapter := &testAdapter{chainID: "resp_1", chainResumable: true}
	gateway := newTestGateway(t, adapter, func(config *Config) { config.ServerState = true })
	if got := gateway.applyResponseChain(nil); got != nil {
		t.Fatalf("nil history = %#v", got)
	}
	if len(adapter.invalidations) == 0 {
		t.Fatal("empty history did not invalidate the chain")
	}
}

func TestRequestHeadersPerFormat(t *testing.T) {
	t.Setenv("GATEWAY_TEST_KEY", "from-env")
	cases := []struct {
		name   string
		format APIFormat
		config func(*Config)
		want   map[string]string
		absent []string
	}{
		{
			name:   "anthropic uses the version header and the key header",
			format: FormatAnthropicMessages,
			config: func(config *Config) { config.APIKey = "" },
			want:   map[string]string{"x-api-key": "from-env", "anthropic-version": DefaultAnthropicVersion},
		},
		{
			name:   "openai completions uses a bearer token",
			format: FormatOpenAICompletions,
			want:   map[string]string{"Authorization": "Bearer secret"},
			absent: []string{"x-api-key", "anthropic-version"},
		},
		{
			name:   "openai responses uses a bearer token",
			format: FormatOpenAIResponses,
			want:   map[string]string{"Authorization": "Bearer secret"},
		},
		{
			name:   "anthropic version can be overridden",
			format: FormatAnthropicMessages,
			config: func(config *Config) { config.AnthropicVersion = "2099-01-01" },
			want:   map[string]string{"anthropic-version": "2099-01-01", "x-api-key": "secret"},
		},
		{
			name:   "an unknown format sends only custom headers",
			format: testFormat,
			config: func(config *Config) {
				config.APIKey = ""
				config.APIKeyEnv = "GATEWAY_TEST_MISSING"
				config.Headers = map[string]string{"X-Custom": "1"}
			},
			want:   map[string]string{"X-Custom": "1"},
			absent: []string{"Authorization", "x-api-key", "anthropic-version"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := Config{
				Format:    test.format,
				Endpoint:  "http://127.0.0.1:1",
				APIKey:    "secret",
				APIKeyEnv: "GATEWAY_TEST_KEY",
			}
			if test.config != nil {
				test.config(&config)
			}
			gateway, err := NewWithAdapter(config, &testAdapter{format: test.format})
			if err != nil {
				t.Fatal(err)
			}
			headers := gateway.requestHeaders()
			for name, value := range test.want {
				if headers[name] != value {
					t.Fatalf("header %q = %q, want %q", name, headers[name], value)
				}
			}
			for _, name := range test.absent {
				if _, exists := headers[name]; exists {
					t.Fatalf("header %q should be absent", name)
				}
			}
		})
	}
}

func TestIsRetryableStatus(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504} {
		if !isRetryableStatus(status) {
			t.Fatalf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{200, 400, 401, 404, 422} {
		if isRetryableStatus(status) {
			t.Fatalf("status %d should not be retryable", status)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "value", "other"); got != "value" {
		t.Fatalf("first non-empty = %q", got)
	}
	if got := firstNonEmpty(" ", ""); got != "" {
		t.Fatalf("first non-empty = %q", got)
	}
}
