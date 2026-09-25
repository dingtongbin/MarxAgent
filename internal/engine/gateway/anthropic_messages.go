// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"sort"
	"strings"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func init() {
	if err := registerAdapter(FormatAnthropicMessages, func() APIAdapter {
		return &anthropicMessagesAdapter{}
	}); err != nil {
		panic(err)
	}
}

// anthropicMessagesAdapter speaks POST /v1/messages. Three rules shape the whole
// adapter: tool results live inside the following user message, tool inputs are
// JSON objects rather than strings, and a thinking block carries a signature
// that must be returned byte for byte or the next turn is rejected.
type anthropicMessagesAdapter struct {
	mu    sync.Mutex
	usage Usage
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	Messages    []anthropicMessage `json:"messages"`
	System      any                `json:"system,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	MaxTokens   int                `json:"max_tokens"`
	Thinking    *anthropicThinking `json:"thinking,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	Stream      bool               `json:"stream"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	Source       *anthropicMedia        `json:"source,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        json.RawMessage        `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      any                    `json:"content,omitempty"`
	IsError      bool                   `json:"is_error,omitempty"`
	Thinking     string                 `json:"thinking,omitempty"`
	Signature    string                 `json:"signature,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicMedia struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicTool struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  json.RawMessage        `json:"input_schema,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

func (a *anthropicMessagesAdapter) Format() APIFormat { return FormatAnthropicMessages }

func (a *anthropicMessagesAdapter) BuildRequest(universal *core.ChatRequest) (any, error) {
	if universal == nil {
		return nil, fmt.Errorf("%w: request must not be nil", ErrInvalidRequest)
	}
	if err := ValidateRequest(universal); err != nil {
		return nil, err
	}
	maxTokens := universal.MaxTokens
	if maxTokens <= 0 {
		// The format has no default and rejects a missing bound, so a request
		// without one gets the documented minimum rather than failing.
		maxTokens = 1
	}
	payload := &anthropicRequest{
		Model:     universal.Model,
		MaxTokens: maxTokens,
		Stream:    true,
	}
	if universal.Temperature > 0 {
		temperature := universal.Temperature
		payload.Temperature = &temperature
	}
	if system := SystemText(universal); system != "" {
		payload.System = system
	}
	for _, message := range ConversationMessages(universal) {
		converted, err := anthropicMessages(message)
		if err != nil {
			return nil, err
		}
		payload.Messages = append(payload.Messages, converted...)
	}
	for _, spec := range universal.Tools {
		schema := spec.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		payload.Tools = append(payload.Tools, anthropicTool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: schema,
		})
	}
	return payload, nil
}

// anthropicMessages converts one universal message. A tool result message becomes
// a user message whose blocks are tool_result entries, which is the only place
// the format accepts them.
func anthropicMessages(message core.Message) ([]anthropicMessage, error) {
	switch message.Role {
	case core.RoleUser:
		blocks, err := anthropicUserBlocks(message.Content)
		if err != nil {
			return nil, err
		}
		return []anthropicMessage{{Role: "user", Content: blocks}}, nil
	case core.RoleAssistant:
		blocks, err := anthropicAssistantBlocks(message.Content)
		if err != nil {
			return nil, err
		}
		return []anthropicMessage{{Role: "assistant", Content: blocks}}, nil
	case core.RoleTool:
		results := ToolResultBlocks(message)
		if len(results) != len(message.Content) {
			return nil, fmt.Errorf("%w: tool message %q carries a non-result block", ErrInvalidRequest, message.ID)
		}
		if len(results) == 0 {
			return nil, fmt.Errorf("%w: tool message %q has no result block", ErrInvalidRequest, message.ID)
		}
		blocks := make([]anthropicBlock, 0, len(results))
		for _, result := range results {
			content := string(result.Output)
			if result.IsError {
				blocks = append(blocks, anthropicBlock{
					Type:      "tool_result",
					ToolUseID: result.ToolCallID,
					Content:   content,
					IsError:   true,
				})
				continue
			}
			blocks = append(blocks, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: result.ToolCallID,
				Content:   content,
			})
		}
		return []anthropicMessage{{Role: "user", Content: blocks}}, nil
	default:
		return nil, fmt.Errorf("%w: role %q cannot be sent to %s", ErrInvalidRequest, message.Role, FormatAnthropicMessages)
	}
}

func anthropicUserBlocks(blocks []core.ContentBlock) ([]anthropicBlock, error) {
	converted := make([]anthropicBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case core.ContentTypeText, core.ContentTypeStructured:
			converted = append(converted, anthropicBlock{Type: "text", Text: block.Text})
		case core.ContentTypeImage:
			mediaType := NormalizeMediaType(block.MediaFormat)
			if mediaType == "" {
				return nil, fmt.Errorf("%w: unsupported image media type %q", ErrInvalidRequest, block.MediaFormat)
			}
			converted = append(converted, anthropicBlock{
				Type:   "image",
				Source: &anthropicMedia{Type: "base64", MediaType: mediaType, Data: block.ImageData},
			})
		case core.ContentTypeToolResult:
			// A caller may inline a result next to the user text, which the format
			// allows because tool_result is a user block.
			converted = append(converted, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: block.ToolCallID,
				Content:   string(block.Output),
				IsError:   block.IsError,
			})
		default:
			return nil, fmt.Errorf("%w: block type %q cannot be sent in a user message", ErrInvalidRequest, block.Type)
		}
	}
	if len(converted) == 0 {
		return nil, fmt.Errorf("%w: user message has no convertible block", ErrInvalidRequest)
	}
	return converted, nil
}

func anthropicAssistantBlocks(blocks []core.ContentBlock) ([]anthropicBlock, error) {
	converted := make([]anthropicBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case core.ContentTypeText, core.ContentTypeStructured:
			converted = append(converted, anthropicBlock{Type: "text", Text: block.Text})
		case core.ContentTypeThinking:
			if block.ThinkingSig == "" {
				// Without a signature the block is rejected on the next turn, so
				// an unsigned block is dropped instead of poisoning the history.
				continue
			}
			converted = append(converted, anthropicBlock{
				Type:      "thinking",
				Thinking:  block.Thinking,
				Signature: block.ThinkingSig,
			})
		case core.ContentTypeToolUse:
			input := block.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			converted = append(converted, anthropicBlock{
				Type:  "tool_use",
				ID:    block.ToolCallID,
				Name:  block.ToolName,
				Input: input,
			})
		default:
			return nil, fmt.Errorf("%w: block type %q cannot be sent in an assistant message", ErrInvalidRequest, block.Type)
		}
	}
	if len(converted) == 0 {
		return nil, fmt.Errorf("%w: assistant message has no convertible block", ErrInvalidRequest)
	}
	return converted, nil
}

// ApplyThinking enables extended thinking. The format requires the output bound
// to exceed the thinking budget, so the request is widened when needed.
func (a *anthropicMessagesAdapter) ApplyThinking(payload any, config ThinkingConfig) error {
	converted, ok := payload.(*anthropicRequest)
	if !ok {
		return fmt.Errorf("%w: anthropic payload is %T", ErrInvalidGateway, payload)
	}
	if !config.Enabled {
		converted.Thinking = nil
		return nil
	}
	budget := config.BudgetTokens
	if budget <= 0 {
		return fmt.Errorf("%w: anthropic thinking requires a positive budget", ErrInvalidGateway)
	}
	converted.Thinking = &anthropicThinking{Type: "enabled", BudgetTokens: budget}
	if converted.MaxTokens <= budget {
		converted.MaxTokens = budget * 2
	}
	return nil
}

func (a *anthropicMessagesAdapter) NormalizeUsage(raw map[string]any) Usage {
	usage := Usage{
		InputTokens:  intFromMap(raw, "input_tokens"),
		OutputTokens: intFromMap(raw, "output_tokens"),
	}
	usage.CacheReadTokens = intFromMap(raw, "cache_read_input_tokens")
	usage.CacheWriteTokens = intFromMap(raw, "cache_creation_input_tokens")
	return usage
}

func (a *anthropicMessagesAdapter) TakeUsage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	usage := a.usage
	a.usage = Usage{}
	return usage
}

func (a *anthropicMessagesAdapter) ApplyCacheHints(payload any, hints CacheHints) error {
	return anthropicCacheHintMapper{}.ApplyCacheHints(payload, hints)
}

func (a *anthropicMessagesAdapter) ExtractCacheStats(usage Usage) CacheStats {
	return CacheStats{
		ReadTokens:  usage.CacheReadTokens,
		WriteTokens: usage.CacheWriteTokens,
		InputTokens: usage.InputTokens,
	}
}

type anthropicStreamEvent struct {
	Type         string             `json:"type"`
	Index        int                `json:"index"`
	Message      anthropicStreamMsg `json:"message"`
	ContentBlock anthropicBlock     `json:"content_block"`
	Delta        anthropicDelta     `json:"delta"`
	Usage        anthropicUsage     `json:"usage"`
	Error        *anthropicError    `json:"error"`
}

type anthropicStreamMsg struct {
	ID    string         `json:"id"`
	Model string         `json:"model"`
	Usage anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type anthropicDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	StopReason  string `json:"stop_reason"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type anthropicBlockState struct {
	kind      string
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	json      strings.Builder
	id        string
	name      string
	// streamed records that the content was already handed to the consumer as
	// deltas, so the block must not be emitted a second time when it closes.
	streamed bool
}

func (a *anthropicMessagesAdapter) ParseStream(body io.Reader) iter.Seq[core.StreamChunk] {
	return func(yield func(core.StreamChunk) bool) {
		a.parseStream(body, yield)
	}
}

func (a *anthropicMessagesAdapter) parseStream(body io.Reader, yield func(core.StreamChunk) bool) {
	blocks := make(map[int]*anthropicBlockState)
	stopped := false
	emit := func(chunk core.StreamChunk) bool {
		if yield(chunk) {
			return true
		}
		stopped = true
		return false
	}
	release := func(index int) bool {
		state, ok := blocks[index]
		if !ok {
			return true
		}
		delete(blocks, index)
		switch state.kind {
		case "tool_use":
			arguments := strings.TrimSpace(state.json.String())
			if arguments == "" {
				arguments = "{}"
			}
			if !json.Valid([]byte(arguments)) {
				emit(core.StreamChunk{
					Type: core.StreamTypeError,
					Err: fmt.Errorf("gateway: tool use %q has malformed input %q",
						state.name, arguments),
				})
				return false
			}
			return emit(core.StreamChunk{
				Type: core.StreamTypeToolCall,
				ToolCall: &core.ContentBlock{
					Type:       core.ContentTypeToolUse,
					ToolCallID: state.id,
					ToolName:   state.name,
					Input:      json.RawMessage(arguments),
				},
			})
		case "thinking":
			if state.thinking.Len() == 0 && state.signature.Len() == 0 {
				return true
			}
			if state.streamed {
				// The text already reached the consumer; only the signature is
				// still outstanding and it is merged into the same block.
				if state.signature.Len() == 0 {
					return true
				}
				return emit(core.StreamChunk{
					Type:        core.StreamTypeThinking,
					ThinkingSig: state.signature.String(),
				})
			}
			return emit(core.StreamChunk{
				Type:        core.StreamTypeThinking,
				Text:        state.thinking.String(),
				ThinkingSig: state.signature.String(),
			})
		default:
			if state.streamed || state.text.Len() == 0 {
				return true
			}
			return emit(core.StreamChunk{Type: core.StreamTypeText, Text: state.text.String()})
		}
	}
	releaseAll := func() bool {
		indexes := make([]int, 0, len(blocks))
		for index := range blocks {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			if !release(index) {
				return false
			}
		}
		return true
	}
	for event := range DecodeStream(body) {
		if stopped {
			return
		}
		payload := anthropicStreamEvent{}
		if strings.TrimSpace(event.Data) == "" {
			continue
		}
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			emit(core.StreamChunk{
				Type: core.StreamTypeError,
				Err:  fmt.Errorf("gateway: decode %s event: %w", FormatAnthropicMessages, err),
			})
			return
		}
		if payload.Type == "" {
			payload.Type = event.Name
		}
		switch payload.Type {
		case "ping":
		case "message_start":
			if payload.Message.Usage.InputTokens > 0 || payload.Message.Usage.OutputTokens > 0 {
				a.recordUsage(payload.Message.Usage)
			}
		case "content_block_start":
			state := &anthropicBlockState{
				kind: payload.ContentBlock.Type,
				id:   payload.ContentBlock.ID,
				name: payload.ContentBlock.Name,
			}
			switch payload.ContentBlock.Type {
			case "text":
				state.text.WriteString(payload.ContentBlock.Text)
			case "thinking":
				state.thinking.WriteString(payload.ContentBlock.Thinking)
				state.signature.WriteString(payload.ContentBlock.Signature)
			case "tool_use":
				// The format opens a tool block with an empty input object and
				// streams the real arguments as deltas, so only a non-empty
				// start payload is treated as the arguments themselves.
				if trimmed := strings.TrimSpace(string(payload.ContentBlock.Input)); trimmed != "" && trimmed != "{}" {
					state.json.WriteString(trimmed)
				}
			}
			blocks[payload.Index] = state
		case "content_block_delta":
			state, ok := blocks[payload.Index]
			if !ok {
				state = &anthropicBlockState{kind: payload.Delta.Type}
				blocks[payload.Index] = state
			}
			switch payload.Delta.Type {
			case "text_delta":
				// An empty delta carries no content; emitting it would append an
				// empty text block to the assistant message.
				if payload.Delta.Text == "" {
					break
				}
				if !emit(core.StreamChunk{Type: core.StreamTypeText, Text: payload.Delta.Text}) {
					return
				}
				state.streamed = true
				state.text.WriteString(payload.Delta.Text)
			case "thinking_delta":
				if payload.Delta.Thinking == "" {
					break
				}
				if !emit(core.StreamChunk{Type: core.StreamTypeThinking, Text: payload.Delta.Thinking}) {
					return
				}
				state.streamed = true
				state.thinking.WriteString(payload.Delta.Thinking)
			case "signature_delta":
				// The signature arrives after the thinking text and must be
				// attached to the block it belongs to.
				state.signature.WriteString(payload.Delta.Signature)
			case "input_json_delta":
				state.json.WriteString(payload.Delta.PartialJSON)
			}
		case "content_block_stop":
			if !release(payload.Index) {
				return
			}
		case "message_delta":
			if payload.Usage.OutputTokens > 0 || payload.Usage.InputTokens > 0 {
				a.recordUsage(payload.Usage)
			}
		case "message_stop":
			if !releaseAll() {
				return
			}
			emit(core.StreamChunk{Type: core.StreamTypeDone})
			return
		case "error":
			message := payload.Error.Message
			if message == "" {
				message = "provider reported an unspecified error"
			}
			emit(core.StreamChunk{
				Type: core.StreamTypeError,
				Text: message,
				Err:  fmt.Errorf("gateway: %s", message),
			})
			return
		default:
			emit(core.StreamChunk{
				Type: core.StreamTypeError,
				Err: fmt.Errorf("gateway: unsupported %s event %q",
					FormatAnthropicMessages, payload.Type),
			})
			return
		}
	}
	if stopped {
		return
	}
	if !releaseAll() {
		return
	}
	emit(core.StreamChunk{Type: core.StreamTypeDone})
}

func (a *anthropicMessagesAdapter) recordUsage(usage anthropicUsage) {
	a.mu.Lock()
	a.usage = a.usage.Add(Usage{
		InputTokens:      usage.InputTokens,
		OutputTokens:     usage.OutputTokens,
		CacheReadTokens:  usage.CacheReadInputTokens,
		CacheWriteTokens: usage.CacheCreationInputTokens,
	})
	a.mu.Unlock()
}
