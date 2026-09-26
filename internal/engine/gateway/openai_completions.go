// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func init() {
	// A registration failure means two files claim the same format, which is a
	// programming error rather than a runtime condition.
	if err := registerAdapter(FormatOpenAICompletions, func() APIAdapter {
		return &openAICompletionsAdapter{}
	}); err != nil {
		panic(err)
	}
}

// openAICompletionsAdapter speaks POST /v1/chat/completions. Tool arguments are
// JSON strings, tool results are standalone role=tool messages, and the format
// has no notion of a thinking block, so reasoning content is dropped exactly as
// the specification requires.
type openAICompletionsAdapter struct {
	mu    sync.Mutex
	usage Usage
}

type completionsRequest struct {
	Model               string               `json:"model"`
	Messages            []completionsMessage `json:"messages"`
	Tools               []completionsTool    `json:"tools,omitempty"`
	MaxCompletionTokens int                  `json:"max_completion_tokens,omitempty"`
	Temperature         *float64             `json:"temperature,omitempty"`
	Stream              bool                 `json:"stream"`
	StreamOptions       *completionsStream   `json:"stream_options,omitempty"`
	ReasoningEffort     string               `json:"reasoning_effort,omitempty"`
}

type completionsStream struct {
	IncludeUsage bool `json:"include_usage"`
}

type completionsMessage struct {
	Role       string                `json:"role"`
	Content    any                   `json:"content"`
	ToolCalls  []completionsToolCall `json:"tool_calls,omitempty"`
	ToolCallID string                `json:"tool_call_id,omitempty"`
	Name       string                `json:"name,omitempty"`
}

type completionsContentPart struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	ImageURL *completionsImage `json:"image_url,omitempty"`
}

type completionsImage struct {
	URL string `json:"url"`
}

type completionsToolCall struct {
	Index    *int                     `json:"index,omitempty"`
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type,omitempty"`
	Function completionsToolCallInner `json:"function"`
}

type completionsToolCallInner struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type completionsTool struct {
	Type     string                 `json:"type"`
	Function completionsToolInnerFn `json:"function"`
}

type completionsToolInnerFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func (a *openAICompletionsAdapter) Format() APIFormat { return FormatOpenAICompletions }

// BuildRequest returns a mutable payload so the thinking and cache hint mappers
// can rewrite it in place before it is marshaled.
func (a *openAICompletionsAdapter) BuildRequest(universal *core.ChatRequest) (any, error) {
	if universal == nil {
		return nil, fmt.Errorf("%w: request must not be nil", ErrInvalidRequest)
	}
	if err := ValidateRequest(universal); err != nil {
		return nil, err
	}
	payload := &completionsRequest{
		Model:               universal.Model,
		Stream:              true,
		StreamOptions:       &completionsStream{IncludeUsage: true},
		MaxCompletionTokens: universal.MaxTokens,
	}
	if universal.Temperature > 0 {
		temperature := universal.Temperature
		payload.Temperature = &temperature
	}
	if system := SystemText(universal); system != "" {
		payload.Messages = append(payload.Messages, completionsMessage{Role: "system", Content: system})
	}
	for _, message := range ConversationMessages(universal) {
		converted, err := completionsMessages(message)
		if err != nil {
			return nil, err
		}
		payload.Messages = append(payload.Messages, converted...)
	}
	for _, spec := range universal.Tools {
		parameters := spec.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		payload.Tools = append(payload.Tools, completionsTool{
			Type: "function",
			Function: completionsToolInnerFn{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  parameters,
			},
		})
	}
	return payload, nil
}

// completionsMessages converts one universal message. A tool result message
// becomes exactly one role=tool message per result, because the format rejects a
// batched result and requires the order to match the calls.
func completionsMessages(message core.Message) ([]completionsMessage, error) {
	switch message.Role {
	case core.RoleUser:
		content, err := completionsUserContent(message.Content)
		if err != nil {
			return nil, err
		}
		return []completionsMessage{{Role: "user", Content: content}}, nil
	case core.RoleAssistant:
		return completionsAssistantMessage(message)
	case core.RoleTool:
		messages := make([]completionsMessage, 0, len(message.Content))
		for _, block := range message.Content {
			if block.Type != core.ContentTypeToolResult {
				continue
			}
			messages = append(messages, completionsMessage{
				Role:       "tool",
				ToolCallID: block.ToolCallID,
				Content:    string(block.Output),
				Name:       block.ToolName,
			})
		}
		if len(messages) == 0 {
			return nil, fmt.Errorf("%w: tool message %q has no result block", ErrInvalidRequest, message.ID)
		}
		return messages, nil
	default:
		return nil, fmt.Errorf("%w: role %q cannot be sent to %s", ErrInvalidRequest, message.Role, FormatOpenAICompletions)
	}
}

func completionsUserContent(blocks []core.ContentBlock) (any, error) {
	parts := make([]completionsContentPart, 0, len(blocks))
	hasImage := false
	var text strings.Builder
	for _, block := range blocks {
		switch block.Type {
		case core.ContentTypeText, core.ContentTypeStructured:
			text.WriteString(block.Text)
		case core.ContentTypeImage:
			mediaType := NormalizeMediaType(block.MediaFormat)
			if mediaType == "" {
				return nil, fmt.Errorf("%w: unsupported image media type %q", ErrInvalidRequest, block.MediaFormat)
			}
			hasImage = true
			parts = append(parts, completionsContentPart{
				Type:     "image_url",
				ImageURL: &completionsImage{URL: "data:" + mediaType + ";base64," + block.ImageData},
			})
		default:
			// Thinking and reasoning have no representation here, and the format
			// drops them silently by design.
		}
	}
	if hasImage {
		if text.Len() > 0 {
			parts = append([]completionsContentPart{{Type: "text", Text: text.String()}}, parts...)
		}
		return parts, nil
	}
	return text.String(), nil
}

func completionsAssistantMessage(message core.Message) ([]completionsMessage, error) {
	var text strings.Builder
	calls := make([]completionsToolCall, 0, len(message.Content))
	for _, block := range message.Content {
		switch block.Type {
		case core.ContentTypeText, core.ContentTypeStructured:
			text.WriteString(block.Text)
		case core.ContentTypeToolUse:
			arguments := string(block.Input)
			if arguments == "" {
				arguments = "{}"
			}
			calls = append(calls, completionsToolCall{
				ID:   block.ToolCallID,
				Type: "function",
				Function: completionsToolCallInner{
					Name:      block.ToolName,
					Arguments: arguments,
				},
			})
		}
	}
	if text.Len() == 0 && len(calls) == 0 {
		return nil, fmt.Errorf("%w: assistant message %q has no content", ErrInvalidRequest, message.ID)
	}
	converted := completionsMessage{Role: "assistant", Content: nil}
	if text.Len() > 0 {
		converted.Content = text.String()
	}
	if len(calls) > 0 {
		converted.ToolCalls = calls
	}
	return []completionsMessage{converted}, nil
}

func (a *openAICompletionsAdapter) ApplyThinking(payload any, config ThinkingConfig) error {
	converted, ok := payload.(*completionsRequest)
	if !ok {
		return fmt.Errorf("%w: completions payload is %T", ErrInvalidGateway, payload)
	}
	if !config.Enabled {
		converted.ReasoningEffort = ""
		return nil
	}
	converted.ReasoningEffort = completionsReasoningEffort(config)
	return nil
}

// completionsReasoningEffort maps a thinking budget onto the discrete levels the
// format accepts, because it has no continuous budget parameter.
func completionsReasoningEffort(config ThinkingConfig) string {
	if effort := strings.ToLower(strings.TrimSpace(config.ReasoningEffort)); effort != "" {
		return effort
	}
	switch {
	case config.BudgetTokens <= 0:
		return ""
	case config.BudgetTokens < 4096:
		return "low"
	case config.BudgetTokens < 16384:
		return "medium"
	default:
		return "high"
	}
}

func (a *openAICompletionsAdapter) NormalizeUsage(raw map[string]any) Usage {
	usage := Usage{
		InputTokens:  intFromMap(raw, "prompt_tokens"),
		OutputTokens: intFromMap(raw, "completion_tokens"),
	}
	if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
		usage.CacheReadTokens = intFromMap(details, "cached_tokens")
	}
	if details, ok := raw["completion_tokens_details"].(map[string]any); ok {
		usage.CacheWriteTokens = intFromMap(details, "reasoning_tokens")
	}
	return usage
}

func (a *openAICompletionsAdapter) TakeUsage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	usage := a.usage
	a.usage = Usage{}
	return usage
}

func (a *openAICompletionsAdapter) ApplyCacheHints(any, CacheHints) error { return nil }

func (a *openAICompletionsAdapter) ExtractCacheStats(usage Usage) CacheStats {
	return CacheStats{
		ReadTokens:  usage.CacheReadTokens,
		WriteTokens: usage.CacheWriteTokens,
		InputTokens: usage.InputTokens,
	}
}

type completionsChunk struct {
	ID      string                   `json:"id"`
	Choices []completionsChunkChoice `json:"choices"`
	Usage   map[string]any           `json:"usage"`
	Error   *completionsErrorPayload `json:"error"`
}

type completionsChunkChoice struct {
	Index        int              `json:"index"`
	Delta        completionsDelta `json:"delta"`
	FinishReason string           `json:"finish_reason"`
}

type completionsDelta struct {
	Role             string                `json:"role"`
	Content          string                `json:"content"`
	ReasoningContent string                `json:"reasoning_content"`
	Reasoning        string                `json:"reasoning"`
	ToolCalls        []completionsToolCall `json:"tool_calls"`
}

type completionsErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type completionsToolAccumulator struct {
	id        string
	name      string
	arguments strings.Builder
}

// isStreamSentinel recognizes the end-of-stream marker. Gateways in front of a
// provider sometimes forward it as a JSON string, so both spellings are accepted.
func isStreamSentinel(data string) bool {
	unquoted := strings.Trim(data, `"`)
	return unquoted == "[DONE]"
}

func (a *openAICompletionsAdapter) ParseStream(body io.Reader) iter.Seq[core.StreamChunk] {
	return func(yield func(core.StreamChunk) bool) {
		a.parseStream(body, yield)
	}
}

func (a *openAICompletionsAdapter) parseStream(body io.Reader, yield func(core.StreamChunk) bool) {
	toolCalls := make(map[int]*completionsToolAccumulator)
	order := make([]int, 0, 4)
	stopped := false
	emit := func(chunk core.StreamChunk) bool {
		if yield(chunk) {
			return true
		}
		stopped = true
		return false
	}
	flushToolCalls := func() bool {
		for _, index := range order {
			accumulator, ok := toolCalls[index]
			if !ok || accumulator == nil {
				continue
			}
			arguments := strings.TrimSpace(accumulator.arguments.String())
			if arguments == "" {
				arguments = "{}"
			}
			if !json.Valid([]byte(arguments)) {
				emit(core.StreamChunk{
					Type: core.StreamTypeError,
					Err:  fmt.Errorf("gateway: tool call %q has malformed arguments %q", accumulator.name, arguments),
				})
				return false
			}
			if !emit(core.StreamChunk{
				Type: core.StreamTypeToolCall,
				ToolCall: &core.ContentBlock{
					Type:       core.ContentTypeToolUse,
					ToolCallID: accumulator.id,
					ToolName:   accumulator.name,
					Input:      json.RawMessage(arguments),
				},
			}) {
				return false
			}
		}
		toolCalls = make(map[int]*completionsToolAccumulator)
		order = order[:0]
		return true
	}
	for event := range DecodeStream(body) {
		if stopped {
			return
		}
		data := strings.TrimSpace(event.Data)
		if data == "" {
			continue
		}
		if isStreamSentinel(data) {
			break
		}
		var chunk completionsChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			emit(core.StreamChunk{
				Type: core.StreamTypeError,
				Err:  fmt.Errorf("gateway: decode %s chunk: %w", FormatOpenAICompletions, err),
			})
			return
		}
		if chunk.Error != nil {
			message := chunk.Error.Message
			if message == "" {
				message = "provider reported an unspecified error"
			}
			emit(core.StreamChunk{Type: core.StreamTypeError, Text: message, Err: fmt.Errorf("gateway: %s", message)})
			return
		}
		if len(chunk.Usage) > 0 {
			a.mu.Lock()
			a.usage = a.usage.Add(a.NormalizeUsage(chunk.Usage))
			a.mu.Unlock()
		}
		for _, choice := range chunk.Choices {
			if text := choice.Delta.Content; text != "" {
				if !emit(core.StreamChunk{Type: core.StreamTypeText, Text: text}) {
					return
				}
			}
			if reasoning := firstNonEmpty(choice.Delta.ReasoningContent, choice.Delta.Reasoning); reasoning != "" {
				if !emit(core.StreamChunk{Type: core.StreamTypeThinking, Text: reasoning}) {
					return
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				index := choice.Index
				if call.Index != nil {
					index = *call.Index
				}
				accumulator, ok := toolCalls[index]
				if !ok {
					accumulator = &completionsToolAccumulator{}
					toolCalls[index] = accumulator
					order = append(order, index)
				}
				if call.ID != "" {
					accumulator.id = call.ID
				}
				if call.Function.Name != "" {
					accumulator.name = call.Function.Name
				}
				accumulator.arguments.WriteString(call.Function.Arguments)
			}
			if choice.FinishReason == "tool_calls" {
				if !flushToolCalls() {
					return
				}
			}
		}
	}
	if stopped {
		return
	}
	if !flushToolCalls() {
		return
	}
	emit(core.StreamChunk{Type: core.StreamTypeDone})
}
