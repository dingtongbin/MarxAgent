// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"sort"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// APIFormat names one of the three supported chat API dialects.
type APIFormat string

const (
	// FormatOpenAICompletions is POST /v1/chat/completions.
	FormatOpenAICompletions APIFormat = "openai-completions"
	// FormatAnthropicMessages is POST /v1/messages.
	FormatAnthropicMessages APIFormat = "anthropic-messages"
	// FormatOpenAIResponses is POST /v1/responses.
	FormatOpenAIResponses APIFormat = "openai-responses"
)

// ErrUnsupportedFormat reports an unknown API format.
type ErrUnsupportedFormat struct {
	Format APIFormat
}

func (e *ErrUnsupportedFormat) Error() string {
	return fmt.Sprintf("gateway: unsupported API format %q", e.Format)
}

// APIAdapter converts between the universal model and one API format. Every
// method is side-effect free so a request can be translated and inspected
// without a network call.
type APIAdapter interface {
	// Format reports which dialect the adapter speaks.
	Format() APIFormat
	// BuildRequest translates a universal request into a format payload.
	BuildRequest(universal *core.ChatRequest) (any, error)
	// ParseStream normalizes a provider event stream.
	ParseStream(body io.Reader) iter.Seq[core.StreamChunk]
	// NormalizeUsage maps a raw usage object onto the universal counters.
	NormalizeUsage(raw map[string]any) Usage
}

// ChainAdapter is implemented by formats that keep server-side conversation
// state. The gateway asks the adapter whether the chain may be resumed before it
// sends a request.
type ChainAdapter interface {
	// ChainUsage reports whether the request continues a server-side chain.
	ChainUsage(request *core.ChatRequest) (previousResponseID string, resumable bool)
	// RecordChain stores the identifier returned by a completed response.
	RecordChain(responseID string)
	// InvalidateChain forgets the server-side chain, forcing a full resend.
	InvalidateChain(reason string)
}

// ThinkingAdapter is implemented by formats with a first-class thinking or
// reasoning control. Formats without one drop the request silently, as their
// specification requires.
type ThinkingAdapter interface {
	ApplyThinking(payload any, config ThinkingConfig) error
}

// UsageReporter is implemented by adapters that report token counters once a
// stream finishes. Usage describes a completed response rather than an
// individual stream event, so it is read after the stream instead of being
// smuggled through core.StreamChunk.
type UsageReporter interface {
	TakeUsage() Usage
}

// ServerStateAdapter is implemented by formats that can keep conversation state
// on the provider side. The gateway enables it only when the configuration asks
// for it, because a resumed chain cannot be audited from the local history alone.
type ServerStateAdapter interface {
	EnableServerState(enabled bool)
}

type adapterFactory func() APIAdapter

var adapterFactories = map[APIFormat]adapterFactory{}

// registerAdapter adds a format to the registry. Adapters call it from an
// initializer so importing the package is enough to make the format available.
func registerAdapter(format APIFormat, factory adapterFactory) error {
	if format == "" {
		return fmt.Errorf("%w: format must not be empty", ErrInvalidGateway)
	}
	if factory == nil {
		return fmt.Errorf("%w: adapter factory must not be nil", ErrInvalidGateway)
	}
	if _, exists := adapterFactories[format]; exists {
		return fmt.Errorf("%w: format %q is already registered", ErrInvalidGateway, format)
	}
	adapterFactories[format] = factory
	return nil
}

// NewAdapter builds a fresh adapter for a format. Adapters are cheap and are not
// shared between gateways because some of them keep chain state.
func NewAdapter(format APIFormat) (APIAdapter, error) {
	factory, exists := adapterFactories[format]
	if !exists {
		return nil, &ErrUnsupportedFormat{Format: format}
	}
	return factory(), nil
}

// Formats lists every supported API format in a stable order.
func Formats() []APIFormat {
	formats := make([]APIFormat, 0, len(adapterFactories))
	for format := range adapterFactories {
		formats = append(formats, format)
	}
	sort.Slice(formats, func(left, right int) bool { return formats[left] < formats[right] })
	return formats
}

// Usage is the provider-neutral token accounting of one response.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// Add accumulates another usage record.
func (u Usage) Add(other Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + other.InputTokens,
		OutputTokens:     u.OutputTokens + other.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + other.CacheWriteTokens,
	}
}

// CacheStats normalizes cache accounting across formats.
type CacheStats struct {
	ReadTokens  int
	WriteTokens int
	InputTokens int
}

// HitRate reports the share of input tokens served from the prefix cache.
func (s CacheStats) HitRate() float64 {
	if s.InputTokens <= 0 {
		return 0
	}
	return float64(s.ReadTokens) / float64(s.InputTokens)
}

// CacheHints is the L2 cache intent. The gateway decides where a cache boundary
// is worth placing and a per-format mapper translates that intent.
type CacheHints struct {
	// StablePrefix marks the system prompt and tool declarations.
	StablePrefix bool
	// HistoryPrefix marks the append-only message history.
	HistoryPrefix bool
	// DynamicTail marks content injected for this turn only, which must never be
	// cached.
	DynamicTail bool
	// MaxBreakpoints is the provider limit for explicit breakpoints.
	MaxBreakpoints int
}

// DefaultCacheBreakpoints is the Anthropic limit of explicit cache breakpoints.
const DefaultCacheBreakpoints = 4

// CacheHintMapper translates the unified cache intent into a format payload and
// reads cache counters back out of a usage record.
type CacheHintMapper interface {
	// ApplyCacheHints rewrites a payload produced by BuildRequest in place.
	ApplyCacheHints(payload any, hints CacheHints) error
	// ExtractCacheStats normalizes cache counters from a usage record.
	ExtractCacheStats(usage Usage) CacheStats
}

// identityCacheHintMapper is correct for formats whose cache behaviour is
// automatic: every prefix byte that stays stable is a cache hit, and the caller
// expresses dynamic content by appending it to the newest message.
type identityCacheHintMapper struct{}

func (identityCacheHintMapper) ApplyCacheHints(any, CacheHints) error { return nil }

func (identityCacheHintMapper) ExtractCacheStats(usage Usage) CacheStats {
	return CacheStats{
		ReadTokens:  usage.CacheReadTokens,
		WriteTokens: usage.CacheWriteTokens,
		InputTokens: usage.InputTokens,
	}
}

func intFromMap(raw map[string]any, keys ...string) int {
	for _, key := range keys {
		value, exists := raw[key]
		if !exists {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return int(typed)
		case int:
			return typed
		case int64:
			return int(typed)
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return int(parsed)
			}
		}
	}
	return 0
}

func mapFromRaw(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}
