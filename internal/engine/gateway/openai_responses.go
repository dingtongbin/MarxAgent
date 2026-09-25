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
	if err := registerAdapter(FormatOpenAIResponses, func() APIAdapter {
		return &openAIResponsesAdapter{}
	}); err != nil {
		panic(err)
	}
}

// openAIResponsesAdapter speaks POST /v1/responses. Two properties shape it: the
// format keeps conversation state on the server, so a turn may carry only the
// items the server has not seen, and reasoning items carry an encrypted blob that
// must be replayed or the chain breaks.
type openAIResponsesAdapter struct {
	mu       sync.Mutex
	usage    Usage
	previous string
	enabled  bool
	// sent records how many items the last request carried, which lets the
	// adapter tell a genuine extension of the chain from a rewritten history.
	sent int
}

type responsesRequest struct {
	Model              string              `json:"model"`
	Instructions       string              `json:"instructions,omitempty"`
	Input              []responsesItem     `json:"input"`
	Tools              []responsesTool     `json:"tools,omitempty"`
	MaxOutputTokens    int                 `json:"max_output_tokens,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
	Stream             bool                `json:"stream"`
	Reasoning          *responsesReasoning `json:"reasoning,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
	Store              bool                `json:"store,omitempty"`
}

type responsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type responsesItem struct {
	Type             string             `json:"type"`
	Role             string             `json:"role,omitempty"`
	Content          []responsesContent `json:"content,omitempty"`
	CallID           string             `json:"call_id,omitempty"`
	Name             string             `json:"name,omitempty"`
	Arguments        string             `json:"arguments,omitempty"`
	Output           string             `json:"output,omitempty"`
	ID               string             `json:"id,omitempty"`
	Summary          []responsesSummary `json:"summary,omitempty"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
}

type responsesSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responsesContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func (a *openAIResponsesAdapter) Format() APIFormat { return FormatOpenAIResponses }

func (a *openAIResponsesAdapter) BuildRequest(universal *core.ChatRequest) (any, error) {
	if universal == nil {
		return nil, fmt.Errorf("%w: request must not be nil", ErrInvalidRequest)
	}
	if err := ValidateRequest(universal); err != nil {
		return nil, err
	}
	payload := &responsesRequest{
		Model:  universal.Model,
		Stream: true,
		Store:  true,
	}
	if universal.Temperature > 0 {
		temperature := universal.Temperature
		payload.Temperature = &temperature
	}
	if universal.MaxTokens > 0 {
		payload.MaxOutputTokens = universal.MaxTokens
	}
	if instructions := SystemText(universal); instructions != "" {
		payload.Instructions = instructions
	}
	for _, spec := range universal.Tools {
		parameters := spec.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		payload.Tools = append(payload.Tools, responsesTool{
			Type:        "function",
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  parameters,
		})
	}
	for _, message := range ConversationMessages(universal) {
		items, err := responsesItems(message)
		if err != nil {
			return nil, err
		}
		payload.Input = append(payload.Input, items...)
	}
	a.mu.Lock()
	if a.enabled && a.previous != "" {
		payload.PreviousResponseID = a.previous
	}
	a.sent = len(payload.Input)
	a.mu.Unlock()
	return payload, nil
}

// responsesItems converts one universal message into format items. A tool result
// becomes a function_call_output item and a tool call becomes a function_call
// item whose arguments are a JSON string.
func responsesItems(message core.Message) ([]responsesItem, error) {
	switch message.Role {
	case core.RoleUser:
		return responsesUserItems(message)
	case core.RoleAssistant:
		return responsesAssistantItems(message)
	case core.RoleTool:
		results := ToolResultBlocks(message)
		if len(results) != len(message.Content) || len(results) == 0 {
			return nil, fmt.Errorf("%w: tool message %q has no result block", ErrInvalidRequest, message.ID)
		}
		items := make([]responsesItem, 0, len(results))
		for _, result := range results {
			items = append(items, responsesItem{
				Type:   "function_call_output",
				CallID: result.ToolCallID,
				Output: string(result.Output),
			})
		}
		return items, nil
	default:
		return nil, fmt.Errorf("%w: role %q cannot be sent to %s", ErrInvalidRequest, message.Role, FormatOpenAIResponses)
	}
}

func responsesUserItems(message core.Message) ([]responsesItem, error) {
	content := make([]responsesContent, 0, len(message.Content))
	for _, block := range message.Content {
		switch block.Type {
		case core.ContentTypeText, core.ContentTypeStructured:
			if block.Text == "" {
				continue
			}
			content = append(content, responsesContent{Type: "input_text", Text: block.Text})
		case core.ContentTypeImage:
			mediaType := NormalizeMediaType(block.MediaFormat)
			if mediaType == "" {
				return nil, fmt.Errorf("%w: unsupported image media type %q", ErrInvalidRequest, block.MediaFormat)
			}
			content = append(content, responsesContent{
				Type:     "input_image",
				ImageURL: "data:" + mediaType + ";base64," + block.ImageData,
			})
		default:
			return nil, fmt.Errorf("%w: block type %q cannot be sent in a user message", ErrInvalidRequest, block.Type)
		}
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("%w: user message %q has no convertible block", ErrInvalidRequest, message.ID)
	}
	return []responsesItem{{Type: "message", Role: "user", Content: content}}, nil
}

func responsesAssistantItems(message core.Message) ([]responsesItem, error) {
	// Items are emitted in block order, which is the order the model produced
	// them: reasoning precedes the visible text and the tool calls that follow it.
	items := make([]responsesItem, 0, len(message.Content))
	for _, block := range message.Content {
		switch block.Type {
		case core.ContentTypeText, core.ContentTypeStructured:
			if block.Text == "" {
				continue
			}
			items = append(items, responsesItem{
				Type:    "message",
				Role:    "assistant",
				Content: []responsesContent{{Type: "output_text", Text: block.Text}},
			})
		case core.ContentTypeReasoning:
			items = append(items, responsesItem{
				Type:             "reasoning",
				ID:               block.ResponseID,
				EncryptedContent: block.EncryptedContent,
			})
		case core.ContentTypeToolUse:
			arguments := string(block.Input)
			if arguments == "" {
				arguments = "{}"
			}
			items = append(items, responsesItem{
				Type:      "function_call",
				CallID:    block.ToolCallID,
				Name:      block.ToolName,
				Arguments: arguments,
			})
		default:
			// A thinking block has no representation here; the format stores its
			// own reasoning items and the gateway drops them silently.
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: assistant message %q has no convertible block", ErrInvalidRequest, message.ID)
	}
	return items, nil
}

// ApplyThinking maps the thinking budget onto the reasoning effort. The format has
// no budget parameter, only a coarse effort level.
func (a *openAIResponsesAdapter) ApplyThinking(payload any, config ThinkingConfig) error {
	converted, ok := payload.(*responsesRequest)
	if !ok {
		return fmt.Errorf("%w: responses payload is %T", ErrInvalidGateway, payload)
	}
	if !config.Enabled {
		converted.Reasoning = nil
		return nil
	}
	reasoning := &responsesReasoning{Summary: "auto"}
	effort := strings.ToLower(strings.TrimSpace(config.ReasoningEffort))
	if effort == "" {
		switch {
		case config.BudgetTokens < 4096:
			effort = "low"
		case config.BudgetTokens < 16384:
			effort = "medium"
		default:
			effort = "high"
		}
	}
	reasoning.Effort = effort
	converted.Reasoning = reasoning
	return nil
}

// ChainUsage reports the server-side turn this adapter may resume from. Resuming
// is only safe when the conversation is an extension of what was sent, which the
// gateway verifies against the recorded response identifier.
func (a *openAIResponsesAdapter) ChainUsage(*core.ChatRequest) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enabled || a.previous == "" {
		return "", false
	}
	return a.previous, true
}

func (a *openAIResponsesAdapter) RecordChain(responseID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.enabled {
		return
	}
	a.previous = responseID
}

func (a *openAIResponsesAdapter) InvalidateChain(string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.previous = ""
}

// EnableServerState turns on the server-side chain for this gateway.
func (a *openAIResponsesAdapter) EnableServerState(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.enabled = enabled
	if !enabled {
		a.previous = ""
	}
}

// SentItems reports how many items the last built request carried.
func (a *openAIResponsesAdapter) SentItems() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sent
}

func (a *openAIResponsesAdapter) NormalizeUsage(raw map[string]any) Usage {
	usage := Usage{
		InputTokens:  intFromMap(raw, "input_tokens"),
		OutputTokens: intFromMap(raw, "output_tokens"),
	}
	if details, ok := raw["input_tokens_details"].(map[string]any); ok {
		usage.CacheReadTokens = intFromMap(details, "cached_tokens")
	}
	if details, ok := raw["output_tokens_details"].(map[string]any); ok {
		usage.CacheWriteTokens = intFromMap(details, "reasoning_tokens")
	}
	return usage
}

func (a *openAIResponsesAdapter) TakeUsage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	usage := a.usage
	a.usage = Usage{}
	return usage
}

func (a *openAIResponsesAdapter) ApplyCacheHints(any, CacheHints) error { return nil }

func (a *openAIResponsesAdapter) ExtractCacheStats(usage Usage) CacheStats {
	return CacheStats{
		ReadTokens:  usage.CacheReadTokens,
		WriteTokens: usage.CacheWriteTokens,
		InputTokens: usage.InputTokens,
	}
}

type responsesEvent struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	ItemID      string          `json:"item_id"`
	Delta       string          `json:"delta"`
	Item        responsesItem   `json:"item"`
	Response    responsesResp   `json:"response"`
	Error       *anthropicError `json:"error"`
	Sequence    int             `json:"sequence_number"`
}

type responsesResp struct {
	ID     string         `json:"id"`
	Status string         `json:"status"`
	Usage  map[string]any `json:"usage"`
}

type responsesToolState struct {
	itemID    string
	callID    string
	name      string
	arguments strings.Builder
	index     int
}

func (a *openAIResponsesAdapter) ParseStream(body io.Reader) iter.Seq[core.StreamChunk] {
	return func(yield func(core.StreamChunk) bool) {
		a.parseStream(body, yield)
	}
}

func (a *openAIResponsesAdapter) parseStream(body io.Reader, yield func(core.StreamChunk) bool) {
	tools := make(map[string]*responsesToolState)
	byIndex := make(map[int]*responsesToolState)
	order := make([]int, 0, 2)
	responseID := ""
	stopped := false
	emit := func(chunk core.StreamChunk) bool {
		if yield(chunk) {
			return true
		}
		stopped = true
		return false
	}
	flush := func(state *responsesToolState) bool {
		arguments := strings.TrimSpace(state.arguments.String())
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			emit(core.StreamChunk{
				Type: core.StreamTypeError,
				Err: fmt.Errorf("gateway: function call %q has malformed arguments %q",
					state.name, arguments),
			})
			return false
		}
		callID := state.callID
		if callID == "" {
			callID = state.itemID
		}
		return emit(core.StreamChunk{
			Type: core.StreamTypeToolCall,
			ToolCall: &core.ContentBlock{
				Type:       core.ContentTypeToolUse,
				ToolCallID: callID,
				ToolName:   state.name,
				Input:      json.RawMessage(arguments),
			},
		})
	}
	flushAll := func() bool {
		for _, index := range order {
			if !flush(byIndex[index]) {
				return false
			}
		}
		tools = make(map[string]*responsesToolState)
		byIndex = make(map[int]*responsesToolState)
		order = order[:0]
		return true
	}
	for event := range DecodeStream(body) {
		if stopped {
			return
		}
		payload := responsesEvent{}
		if strings.TrimSpace(event.Data) == "" {
			continue
		}
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			emit(core.StreamChunk{
				Type: core.StreamTypeError,
				Err:  fmt.Errorf("gateway: decode %s event: %w", FormatOpenAIResponses, err),
			})
			return
		}
		if payload.Type == "" {
			payload.Type = event.Name
		}
		switch payload.Type {
		case "response.created", "response.in_progress":
			if payload.Response.ID != "" {
				responseID = payload.Response.ID
			}
		case "response.output_text.delta":
			if payload.Delta == "" {
				break
			}
			if !emit(core.StreamChunk{Type: core.StreamTypeText, Text: payload.Delta}) {
				return
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if payload.Delta == "" {
				break
			}
			if !emit(core.StreamChunk{Type: core.StreamTypeThinking, Text: payload.Delta}) {
				return
			}
		case "response.output_item.added":
			if payload.Item.Type != "function_call" {
				break
			}
			state := &responsesToolState{
				itemID: firstNonEmpty(payload.Item.ID, payload.ItemID),
				callID: payload.Item.CallID,
				name:   payload.Item.Name,
				index:  payload.OutputIndex,
			}
			if trimmed := strings.TrimSpace(payload.Item.Arguments); trimmed != "" && trimmed != "{}" {
				state.arguments.WriteString(trimmed)
			}
			tools[state.itemID] = state
			byIndex[state.index] = state
			order = append(order, state.index)
		case "response.function_call_arguments.delta":
			state, ok := tools[payload.ItemID]
			if !ok {
				state = &responsesToolState{itemID: payload.ItemID, index: payload.OutputIndex}
				tools[payload.ItemID] = state
				byIndex[state.index] = state
				order = append(order, state.index)
			}
			state.arguments.WriteString(payload.Delta)
		case "response.output_item.done":
			state, ok := byIndex[payload.OutputIndex]
			if !ok {
				break
			}
			if trimmed := strings.TrimSpace(payload.Item.Arguments); trimmed != "" && trimmed != "{}" {
				state.arguments.Reset()
				state.arguments.WriteString(trimmed)
			}
			if payload.Item.Name != "" {
				state.name = payload.Item.Name
			}
			if payload.Item.CallID != "" {
				state.callID = payload.Item.CallID
			}
		case "response.completed", "response.incomplete":
			if payload.Response.ID != "" {
				responseID = payload.Response.ID
			}
			if len(payload.Response.Usage) > 0 {
				a.mu.Lock()
				a.usage = a.usage.Add(a.NormalizeUsage(payload.Response.Usage))
				a.mu.Unlock()
			}
			if !flushAll() {
				return
			}
			emit(core.StreamChunk{Type: core.StreamTypeDone, ResponseID: responseID})
			return
		case "response.failed", "error":
			message := "provider reported an unspecified error"
			if payload.Error != nil && payload.Error.Message != "" {
				message = payload.Error.Message
			}
			emit(core.StreamChunk{
				Type: core.StreamTypeError,
				Text: message,
				Err:  fmt.Errorf("gateway: %s", message),
			})
			return
		case "response.content_part.added", "response.content_part.done",
			"response.output_text.done", "response.reasoning_summary_text.done",
			"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		default:
			emit(core.StreamChunk{
				Type: core.StreamTypeError,
				Err: fmt.Errorf("gateway: unsupported %s event %q",
					FormatOpenAIResponses, payload.Type),
			})
			return
		}
	}
	if stopped {
		return
	}
	if !flushAll() {
		return
	}
	emit(core.StreamChunk{Type: core.StreamTypeDone, ResponseID: responseID})
}
