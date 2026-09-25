// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// DefaultAnthropicVersion is sent on every Anthropic request.
const DefaultAnthropicVersion = "2023-06-01"

// DefaultTimeout bounds a streaming request that produces no events.
const DefaultTimeout = 10 * time.Minute

// ErrInvalidGateway reports an unusable gateway configuration.
var ErrInvalidGateway = errors.New("gateway: invalid configuration")

// ThinkingConfig enables extended thinking or reasoning summaries.
type ThinkingConfig struct {
	Enabled      bool
	BudgetTokens int
	// ReasoningEffort is the OpenAI Chat Completions spelling.
	ReasoningEffort string
}

// Config describes one model endpoint.
type Config struct {
	Format    APIFormat
	Endpoint  string
	APIKey    string
	APIKeyEnv string
	Model     string
	Headers   map[string]string
	// AnthropicVersion overrides the anthropic-version header.
	AnthropicVersion string
	Thinking         *ThinkingConfig
	// ServerState enables server-side conversation state where the format has it.
	ServerState bool
	// MaxRetries bounds transport retries for a request that failed before any
	// content was produced.
	MaxRetries int
	// Timeout bounds the whole streaming call.
	Timeout time.Duration
	// HTTPClient overrides the default client, which tests use.
	HTTPClient *http.Client
}

// ProviderGateway is the only core.Provider implementation. It owns transport,
// authentication, cache hints and the optional server-side response chain, and
// delegates every format detail to an APIAdapter.
type ProviderGateway struct {
	config   Config
	adapter  APIAdapter
	mapper   CacheHintMapper
	client   *http.Client
	usageMu  sync.Mutex
	usage    Usage
	chainMu  sync.Mutex
	chain    responseChain
	inFlight sync.WaitGroup
}

// New builds a gateway for a registered API format.
func New(config Config) (*ProviderGateway, error) {
	adapter, err := NewAdapter(config.Format)
	if err != nil {
		return nil, err
	}
	return NewWithAdapter(config, adapter)
}

// NewWithAdapter builds a gateway around an explicit adapter, which is how tests
// and future formats plug in without touching the registry.
func NewWithAdapter(config Config, adapter APIAdapter) (*ProviderGateway, error) {
	if adapter == nil {
		return nil, fmt.Errorf("%w: adapter must not be nil", ErrInvalidGateway)
	}
	if strings.TrimSpace(config.Endpoint) == "" {
		return nil, fmt.Errorf("%w: endpoint is required for format %q", ErrInvalidGateway, config.Format)
	}
	if _, err := resolveEndpoint(config); err != nil {
		return nil, err
	}
	if config.Timeout <= 0 {
		config.Timeout = DefaultTimeout
	}
	if config.MaxRetries < 0 {
		return nil, fmt.Errorf("%w: max retries must not be negative", ErrInvalidGateway)
	}
	if config.Thinking != nil && config.Thinking.BudgetTokens < 0 {
		return nil, fmt.Errorf("%w: thinking budget must not be negative", ErrInvalidGateway)
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: config.Timeout}
	}
	gateway := &ProviderGateway{
		config:  config,
		adapter: adapter,
		mapper:  mapperFor(config.Format, adapter),
		client:  client,
	}
	if !config.ServerState {
		gateway.chain.disabled = true
	}
	return gateway, nil
}

// Format reports the API format in use.
func (g *ProviderGateway) Format() APIFormat {
	if g == nil {
		return ""
	}
	return g.adapter.Format()
}

// Name identifies the gateway for diagnostics and audit records.
func (g *ProviderGateway) Name() string {
	if g == nil {
		return ""
	}
	return fmt.Sprintf("gateway:%s:%s", g.adapter.Format(), g.config.Model)
}

// Usage reports the accumulated token counters of this gateway.
func (g *ProviderGateway) Usage() Usage {
	if g == nil {
		return Usage{}
	}
	g.usageMu.Lock()
	defer g.usageMu.Unlock()
	return g.usage
}

// ResetUsage clears the accumulated counters.
func (g *ProviderGateway) ResetUsage() {
	if g == nil {
		return
	}
	g.usageMu.Lock()
	g.usage = Usage{}
	g.usageMu.Unlock()
}

// InvalidateChain drops the server-side response chain so the next request sends
// the full history again.
func (g *ProviderGateway) InvalidateChain(reason string) {
	if g == nil {
		return
	}
	g.chainMu.Lock()
	g.chain.invalidate()
	g.chainMu.Unlock()
	if chainAdapter, ok := g.adapter.(ChainAdapter); ok {
		chainAdapter.InvalidateChain(reason)
	}
}

// Close waits for in-flight streams to finish.
func (g *ProviderGateway) Close() {
	if g == nil {
		return
	}
	g.inFlight.Wait()
}

type responseChain struct {
	disabled bool
	previous string
}

func (c *responseChain) invalidate() {
	c.previous = ""
}

func (c *responseChain) snapshot() (string, bool) {
	if c.disabled || c.previous == "" {
		return "", false
	}
	return c.previous, true
}

func (c *responseChain) record(responseID string) {
	if c.disabled || responseID == "" {
		return
	}
	c.previous = responseID
}

// ChatStream converts a universal request into a provider request and normalizes
// the response stream back into core.StreamChunk values.
func (g *ProviderGateway) ChatStream(ctx context.Context, request core.ChatRequest) (<-chan core.StreamChunk, error) {
	if g == nil {
		return nil, errors.New("gateway: nil gateway")
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context must not be nil", ErrInvalidGateway)
	}
	if err := ValidateRequest(&request); err != nil {
		return nil, err
	}

	messages := g.applyResponseChain(CloneMessages(request.Messages))
	universal := &core.ChatRequest{
		Model:       firstNonEmpty(request.Model, g.config.Model),
		Messages:    messages,
		Tools:       CloneTools(request.Tools),
		SystemHint:  request.SystemHint,
		MaxTokens:   request.MaxTokens,
		Temperature: request.Temperature,
		CacheKey:    request.CacheKey,
	}
	payload, err := g.adapter.BuildRequest(universal)
	if err != nil {
		return nil, err
	}
	if err := g.applyFormatExtras(payload); err != nil {
		return nil, err
	}
	if err := g.mapper.ApplyCacheHints(payload, CacheHintsFor(&request)); err != nil {
		return nil, err
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		return nil, err
	}

	streamContext, cancel := context.WithTimeout(ctx, g.config.Timeout)
	body, err := g.send(streamContext, encoded)
	if err != nil {
		cancel()
		return nil, err
	}

	chunks := make(chan core.StreamChunk, 32)
	g.inFlight.Add(1)
	go func() {
		defer g.inFlight.Done()
		defer cancel()
		defer close(chunks)
		defer func() {
			_ = body.Close()
		}()
		g.consume(streamContext, body, chunks)
	}()
	return chunks, nil
}

// applyResponseChain trims history that the provider already holds. The gateway
// state is authoritative: once it has invalidated the chain the adapter's stale
// identifier is ignored, because a partially replayed conversation is worse than
// a full resend.
func (g *ProviderGateway) applyResponseChain(messages []core.Message) []core.Message {
	chainAdapter, isChain := g.adapter.(ChainAdapter)
	g.chainMu.Lock()
	defer g.chainMu.Unlock()
	if g.chain.disabled || !isChain || len(messages) == 0 {
		g.chain.invalidate()
		if isChain {
			chainAdapter.InvalidateChain("history rewritten")
		}
		return messages
	}
	previous, resumable := chainAdapter.ChainUsage(&core.ChatRequest{Messages: messages})
	recorded, hasRecord := g.chain.snapshot()
	if !resumable || previous == "" || !hasRecord {
		return messages
	}
	if previous != recorded {
		g.chain.invalidate()
		chainAdapter.InvalidateChain("adapter chain diverged from the gateway")
		return messages
	}
	trimmed, found := TrimChainHistory(messages, previous)
	if !found {
		g.chain.invalidate()
		chainAdapter.InvalidateChain("chain history not found")
		return messages
	}
	return trimmed
}

func (g *ProviderGateway) applyFormatExtras(payload any) error {
	if g.config.Thinking == nil || !g.config.Thinking.Enabled {
		return nil
	}
	thinkingAdapter, ok := g.adapter.(ThinkingAdapter)
	if !ok {
		return nil
	}
	return thinkingAdapter.ApplyThinking(payload, *g.config.Thinking)
}

func (g *ProviderGateway) send(ctx context.Context, encoded []byte) (io.ReadCloser, error) {
	endpoint, err := resolveEndpoint(g.config)
	if err != nil {
		return nil, err
	}
	attempts := g.config.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		for name, value := range g.requestHeaders() {
			request.Header.Set(name, value)
		}
		response, err := g.client.Do(request)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			detail := ReadErrorBody(response.Body)
			_ = response.Body.Close()
			lastErr = fmt.Errorf("gateway: %s returned HTTP %d: %s", g.adapter.Format(), response.StatusCode, detail)
			if isRetryableStatus(response.StatusCode) && attempt < attempts-1 {
				continue
			}
			return nil, lastErr
		}
		return response.Body, nil
	}
	return nil, fmt.Errorf("gateway: %s request failed: %w", g.adapter.Format(), lastErr)
}

func (g *ProviderGateway) requestHeaders() map[string]string {
	headers := make(map[string]string, len(g.config.Headers)+3)
	for name, value := range g.config.Headers {
		headers[name] = value
	}
	key := g.apiKey()
	switch g.adapter.Format() {
	case FormatAnthropicMessages:
		version := g.config.AnthropicVersion
		if version == "" {
			version = DefaultAnthropicVersion
		}
		headers["anthropic-version"] = version
		if key != "" {
			headers["x-api-key"] = key
		}
	case FormatOpenAICompletions, FormatOpenAIResponses:
		if key != "" {
			headers["Authorization"] = "Bearer " + key
		}
	}
	return headers
}

func (g *ProviderGateway) apiKey() string {
	if g.config.APIKey != "" {
		return g.config.APIKey
	}
	if g.config.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(g.config.APIKeyEnv)
}

func (g *ProviderGateway) consume(ctx context.Context, body io.Reader, chunks chan<- core.StreamChunk) {
	sawTerminal := false
	for chunk := range g.adapter.ParseStream(body) {
		if chunk.Type == core.StreamTypeDone || chunk.Type == core.StreamTypeError {
			sawTerminal = true
		}
		if chunk.ResponseID != "" {
			g.chainMu.Lock()
			g.chain.record(chunk.ResponseID)
			g.chainMu.Unlock()
			if chainAdapter, ok := g.adapter.(ChainAdapter); ok {
				chainAdapter.RecordChain(chunk.ResponseID)
			}
		}
		select {
		case chunks <- chunk:
		case <-ctx.Done():
			return
		}
	}
	if reporter, ok := g.adapter.(UsageReporter); ok {
		g.recordUsage(reporter.TakeUsage())
	}
	if !sawTerminal {
		select {
		case chunks <- core.StreamChunk{Type: core.StreamTypeDone}:
		case <-ctx.Done():
		}
	}
}

func (g *ProviderGateway) recordUsage(usage Usage) {
	g.usageMu.Lock()
	g.usage = g.usage.Add(usage)
	g.usageMu.Unlock()
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func cacheHintMapperFor(format APIFormat) CacheHintMapper {
	return identityCacheHintMapper{}
}

// mapperFor prefers the mapper an adapter ships with, so a format keeps its cache
// translation next to its request builder, and falls back to the format default.
func mapperFor(format APIFormat, adapter APIAdapter) CacheHintMapper {
	if mapper, ok := adapter.(CacheHintMapper); ok {
		return mapper
	}
	return cacheHintMapperFor(format)
}

// resolveEndpoint normalizes a base URL and appends the format specific path. A
// format the gateway does not know keeps the configured endpoint verbatim, which
// is how an injected adapter addresses its own server.
func resolveEndpoint(config Config) (string, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("%w: endpoint is required", ErrInvalidGateway)
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")
	switch config.Format {
	case FormatOpenAICompletions:
		return endpoint + "/chat/completions", nil
	case FormatAnthropicMessages:
		return endpoint + "/messages", nil
	case FormatOpenAIResponses:
		return endpoint + "/responses", nil
	case "":
		return "", fmt.Errorf("%w: format must not be empty", ErrInvalidGateway)
	default:
		return endpoint, nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func marshalPayload(payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("gateway: marshal request payload: %w", err)
	}
	return encoded, nil
}
