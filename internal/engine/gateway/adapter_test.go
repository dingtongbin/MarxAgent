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
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func TestUnsupportedFormatError(t *testing.T) {
	err := &ErrUnsupportedFormat{Format: "nope"}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error = %v", err)
	}
	var target *ErrUnsupportedFormat
	if !errors.As(error(err), &target) || target.Format != "nope" {
		t.Fatalf("errors.As failed for %#v", target)
	}
}

func TestFormatsAreSorted(t *testing.T) {
	registered := []APIFormat{"zzz-test-format", "aaa-test-format"}
	for _, format := range registered {
		if err := registerAdapter(format, func() APIAdapter { return &testAdapter{format: format} }); err != nil {
			t.Fatal(err)
		}
	}
	formats := Formats()
	if len(formats) != len(registered) {
		t.Fatalf("formats = %v", formats)
	}
	for index := 1; index < len(formats); index++ {
		if formats[index-1] >= formats[index] {
			t.Fatalf("formats are not sorted: %v", formats)
		}
	}
}

func TestNewUsesRegisteredFormat(t *testing.T) {
	const format APIFormat = "new-test-format"
	if err := registerAdapter(format, func() APIAdapter { return &testAdapter{format: format} }); err != nil {
		t.Fatal(err)
	}
	gateway, err := New(Config{Format: format, Endpoint: "127.0.0.1:9", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if gateway.Format() != format {
		t.Fatalf("format = %q", gateway.Format())
	}
}

func TestIdentityCacheHintMapper(t *testing.T) {
	mapper := identityCacheHintMapper{}
	if err := mapper.ApplyCacheHints(map[string]any{}, CacheHints{StablePrefix: true}); err != nil {
		t.Fatal(err)
	}
	stats := mapper.ExtractCacheStats(Usage{InputTokens: 10, CacheReadTokens: 4, CacheWriteTokens: 1})
	if stats.ReadTokens != 4 || stats.WriteTokens != 1 || stats.InputTokens != 10 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestMapperForPrefersAdapterMapper(t *testing.T) {
	own := &testAdapter{format: "own-mapper"}
	if got := mapperFor("own-mapper", own); got != CacheHintMapper(own) {
		t.Fatalf("mapper = %#v", got)
	}
	plain := &plainAdapter{}
	if got := mapperFor("plain", plain); got == nil {
		t.Fatal("fallback mapper is nil")
	}
}

type plainAdapter struct{}

func (plainAdapter) Format() APIFormat { return "plain" }

func (plainAdapter) BuildRequest(*core.ChatRequest) (any, error) { return nil, nil }

func (plainAdapter) ParseStream(io.Reader) iter.Seq[core.StreamChunk] {
	return func(func(core.StreamChunk) bool) {}
}

func (plainAdapter) NormalizeUsage(map[string]any) Usage { return Usage{} }

func TestIntFromMap(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		keys []string
		want int
	}{
		{name: "missing", raw: map[string]any{}, keys: []string{"a"}},
		{name: "float", raw: map[string]any{"a": 3.0}, keys: []string{"a"}, want: 3},
		{name: "int", raw: map[string]any{"a": 4}, keys: []string{"a"}, want: 4},
		{name: "int64", raw: map[string]any{"a": int64(5)}, keys: []string{"a"}, want: 5},
		{name: "number", raw: map[string]any{"a": json.Number("6")}, keys: []string{"a"}, want: 6},
		{name: "invalid number", raw: map[string]any{"a": json.Number("x")}, keys: []string{"a"}},
		{name: "string", raw: map[string]any{"a": "7"}, keys: []string{"a"}},
		{name: "nil value", raw: map[string]any{"a": nil}, keys: []string{"a"}},
		{name: "first key wins", raw: map[string]any{"b": 8, "a": 1}, keys: []string{"a", "b"}, want: 1},
		{name: "later key is used", raw: map[string]any{"b": 8}, keys: []string{"a", "b"}, want: 8},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := intFromMap(test.raw, test.keys...); got != test.want {
				t.Fatalf("intFromMap = %d, want %d", got, test.want)
			}
		})
	}
}

func TestMapFromRaw(t *testing.T) {
	if got := mapFromRaw(nil); got != nil {
		t.Fatalf("nil raw = %#v", got)
	}
	if got := mapFromRaw(json.RawMessage(`{`)); got != nil {
		t.Fatalf("invalid raw = %#v", got)
	}
	got := mapFromRaw(json.RawMessage(`{"a":1}`))
	if len(got) != 1 {
		t.Fatalf("raw = %#v", got)
	}
}

func TestMarshalPayloadRejectsUnencodableValues(t *testing.T) {
	if _, err := marshalPayload(make(chan int)); err == nil {
		t.Fatal("channel payload accepted")
	}
	if _, err := marshalPayload(map[string]string{"model": "m"}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteEventRejectsUnencodablePayload(t *testing.T) {
	var builder strings.Builder
	if err := NewStreamWriter(&builder).WriteEvent("x", make(chan int)); err == nil {
		t.Fatal("unencodable payload accepted")
	}
}

func TestStreamWriterFlushWithoutFlusher(t *testing.T) {
	var builder strings.Builder
	if err := NewStreamWriter(&builder).Flush(); err != nil {
		t.Fatal(err)
	}
	if err := NewStreamWriter(nil).Flush(); err == nil {
		t.Fatal("nil target flush accepted")
	}
}

func TestParseStreamFieldRejectsMalformedLines(t *testing.T) {
	if _, _, ok := parseStreamField("no-colon"); ok {
		t.Fatal("line without a colon accepted")
	}
	if _, _, ok := parseStreamField(""); ok {
		t.Fatal("empty line accepted")
	}
	if _, _, ok := parseStreamField(": comment"); ok {
		t.Fatal("comment accepted")
	}
}

func TestReadErrorBodyReturnsEmptyOnFailure(t *testing.T) {
	if got := ReadErrorBody(failingReader{}); got != "" {
		t.Fatalf("failing reader body = %q", got)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestAPIKeyResolution(t *testing.T) {
	t.Setenv("GATEWAY_KEY_TEST", "from-env")
	gateway := &ProviderGateway{config: Config{APIKeyEnv: "GATEWAY_KEY_TEST"}}
	if got := gateway.apiKey(); got != "from-env" {
		t.Fatalf("api key = %q", got)
	}
	gateway = &ProviderGateway{config: Config{APIKey: "inline", APIKeyEnv: "GATEWAY_KEY_TEST"}}
	if got := gateway.apiKey(); got != "inline" {
		t.Fatalf("api key = %q", got)
	}
	gateway = &ProviderGateway{config: Config{APIKeyEnv: "GATEWAY_KEY_MISSING"}}
	if got := gateway.apiKey(); got != "" {
		t.Fatalf("missing api key = %q", got)
	}
	if got := (&ProviderGateway{}).apiKey(); got != "" {
		t.Fatalf("empty api key = %q", got)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial failed")
}

func TestSendReportsTransportFailures(t *testing.T) {
	gateway := &ProviderGateway{
		config:  Config{Format: testFormat, Endpoint: "http://127.0.0.1:1", MaxRetries: 1},
		adapter: &testAdapter{},
		client:  &http.Client{Transport: failingTransport{}},
	}
	if _, err := gateway.send(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("transport failure accepted")
	} else if !strings.Contains(err.Error(), "dial failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestSendStopsOnCanceledContext(t *testing.T) {
	gateway := &ProviderGateway{
		config:  Config{Format: testFormat, Endpoint: "http://127.0.0.1:1"},
		adapter: &testAdapter{},
		client:  &http.Client{Transport: failingTransport{}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gateway.send(ctx, []byte(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled send = %v", err)
	}
}

func TestSendDoesNotRetryClientErrors(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, "bad key")
	}))
	defer server.Close()
	gateway := &ProviderGateway{
		config:  Config{Format: testFormat, Endpoint: server.URL, MaxRetries: 3},
		adapter: &testAdapter{},
		client:  server.Client(),
	}
	if _, err := gateway.send(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("client error accepted")
	} else if !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error = %v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}
