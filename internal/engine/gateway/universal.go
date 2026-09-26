// SPDX-License-Identifier: Apache-2.0

// Package gateway adapts the L1 provider-neutral chat model to the three
// mainstream API formats. The universal model in internal/core is the single
// conversion hub: every request is translated into a format payload by a pure
// function and every provider stream is normalized back into core.StreamChunk.
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// ErrInvalidRequest reports a universal request that no format can represent.
var ErrInvalidRequest = errors.New("gateway: invalid request")

// HoistSystemMessages moves role=system messages out of the history and returns
// their text. Formats that carry the system prompt outside the message list need
// this, and the L1 loop keeps the hint in ChatRequest.SystemHint.
func HoistSystemMessages(messages []core.Message) (string, []core.Message) {
	var system []string
	rest := make([]core.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role != core.RoleSystem {
			rest = append(rest, message)
			continue
		}
		if text := strings.TrimSpace(messageText(message)); text != "" {
			system = append(system, text)
		}
	}
	return strings.Join(system, "\n\n"), rest
}

// SystemText returns the effective system prompt of a request.
func SystemText(request *core.ChatRequest) string {
	if request == nil {
		return ""
	}
	hoisted, _ := HoistSystemMessages(request.Messages)
	parts := make([]string, 0, 2)
	if hint := strings.TrimSpace(request.SystemHint); hint != "" {
		parts = append(parts, hint)
	}
	if hoisted != "" {
		parts = append(parts, hoisted)
	}
	return strings.Join(parts, "\n\n")
}

// ConversationMessages returns the history without system messages, which is the
// only representation every format accepts.
func ConversationMessages(request *core.ChatRequest) []core.Message {
	if request == nil {
		return nil
	}
	_, messages := HoistSystemMessages(request.Messages)
	return messages
}

// ValidateRequest rejects universal requests that cannot be translated without
// losing information, so a malformed request fails before any network call.
func ValidateRequest(request *core.ChatRequest) error {
	if request == nil {
		return fmt.Errorf("%w: request must not be nil", ErrInvalidRequest)
	}
	if strings.TrimSpace(request.Model) == "" {
		return fmt.Errorf("%w: model must not be empty", ErrInvalidRequest)
	}
	if request.MaxTokens < 0 {
		return fmt.Errorf("%w: max tokens must not be negative", ErrInvalidRequest)
	}
	if request.Temperature < 0 || request.Temperature > 2 {
		return fmt.Errorf("%w: temperature %v is out of range", ErrInvalidRequest, request.Temperature)
	}
	declared := make(map[string]struct{}, len(request.Tools))
	for index, spec := range request.Tools {
		if strings.TrimSpace(spec.Name) == "" {
			return fmt.Errorf("%w: tool %d has no name", ErrInvalidRequest, index)
		}
		if len(spec.Parameters) > 0 && !IsJSONObject(spec.Parameters) {
			return fmt.Errorf("%w: tool %q parameters must be a JSON object", ErrInvalidRequest, spec.Name)
		}
		if _, exists := declared[spec.Name]; exists {
			return fmt.Errorf("%w: duplicate tool %q", ErrInvalidRequest, spec.Name)
		}
		declared[spec.Name] = struct{}{}
	}
	for messageIndex, message := range request.Messages {
		if err := validateMessage(message, declared, len(request.Tools) > 0); err != nil {
			return fmt.Errorf("%w: message %d: %v", ErrInvalidRequest, messageIndex, err)
		}
	}
	return nil
}

func validateMessage(message core.Message, declared map[string]struct{}, checkTools bool) error {
	switch message.Role {
	case core.RoleSystem, core.RoleUser, core.RoleAssistant, core.RoleTool:
	default:
		return fmt.Errorf("unsupported role %q", message.Role)
	}
	if len(message.Content) == 0 {
		return errors.New("content must not be empty")
	}
	for blockIndex, block := range message.Content {
		if err := validateBlock(block, declared, checkTools); err != nil {
			return fmt.Errorf("block %d: %v", blockIndex, err)
		}
	}
	if message.Role == core.RoleTool {
		for _, block := range message.Content {
			if block.Type != core.ContentTypeToolResult {
				return fmt.Errorf("tool message carries a %q block", block.Type)
			}
		}
	}
	return nil
}

func validateBlock(block core.ContentBlock, declared map[string]struct{}, checkTools bool) error {
	switch block.Type {
	case core.ContentTypeText, core.ContentTypeStructured:
		return nil
	case core.ContentTypeImage:
		if block.ImageData == "" {
			return errors.New("image block has no data")
		}
		if NormalizeMediaType(block.MediaFormat) == "" {
			return fmt.Errorf("unsupported image media type %q", block.MediaFormat)
		}
		return nil
	case core.ContentTypeThinking:
		if block.Thinking == "" && block.ThinkingSig == "" {
			return errors.New("thinking block has neither text nor a signature")
		}
		return nil
	case core.ContentTypeReasoning:
		if block.EncryptedContent == "" {
			return errors.New("reasoning block has no encrypted content")
		}
		return nil
	case core.ContentTypeToolUse:
		if block.ToolCallID == "" || block.ToolName == "" {
			return errors.New("tool call has no id or name")
		}
		if len(block.Input) > 0 && !IsJSONObject(block.Input) {
			return errors.New("tool call input must be a JSON object")
		}
		if checkTools {
			if _, exists := declared[block.ToolName]; !exists {
				return fmt.Errorf("tool call names undeclared tool %q", block.ToolName)
			}
		}
		return nil
	case core.ContentTypeToolResult:
		if block.ToolCallID == "" {
			return errors.New("tool result has no call id")
		}
		if len(block.Output) == 0 {
			return errors.New("tool result has no output")
		}
		if !json.Valid(block.Output) {
			return errors.New("tool result output is not valid JSON")
		}
		return nil
	default:
		return fmt.Errorf("unsupported content type %q", block.Type)
	}
}

// LastResponseID returns the newest server-side response identifier in the
// history. Only the Responses format can resume from it.
func LastResponseID(messages []core.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != core.RoleAssistant {
			continue
		}
		if value, ok := messages[index].Metadata["response_id"].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// MessageResponseID reads the server-side response identifier of one message.
func MessageResponseID(message core.Message) string {
	if message.Metadata == nil {
		return ""
	}
	value, _ := message.Metadata["response_id"].(string)
	return value
}

// TrimChainHistory returns the messages that follow the assistant turn produced
// by previousResponseID. Those are exactly the items a server-side chain does not
// have yet. The second result is false when the history no longer contains that
// turn, which means the chain cannot be resumed.
func TrimChainHistory(messages []core.Message, previousResponseID string) ([]core.Message, bool) {
	if previousResponseID == "" {
		return nil, false
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if MessageResponseID(messages[index]) == previousResponseID {
			return messages[index+1:], true
		}
	}
	return nil, false
}

// CacheHintsFor derives the unified cache intent from a request shape. The
// gateway decides where a boundary is worth placing; a per-format mapper decides
// how to express it.
func CacheHintsFor(request *core.ChatRequest) CacheHints {
	hints := CacheHints{MaxBreakpoints: DefaultCacheBreakpoints}
	if request == nil {
		return hints
	}
	hints.StablePrefix = strings.TrimSpace(request.SystemHint) != "" || len(request.Tools) > 0
	hints.HistoryPrefix = len(ConversationMessages(request)) > 1
	return hints
}

// NormalizeMediaType maps a bare extension such as "png" to the media type every
// format expects, and rejects unknown values.
func NormalizeMediaType(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	switch trimmed {
	case "":
		return ""
	case "image/png", "png":
		return "image/png"
	case "image/jpeg", "jpeg", "jpg":
		return "image/jpeg"
	case "image/gif", "gif":
		return "image/gif"
	case "image/webp", "webp":
		return "image/webp"
	default:
		return ""
	}
}

// MessageText concatenates the text blocks of a message.
func MessageText(message core.Message) string {
	return messageText(message)
}

func messageText(message core.Message) string {
	var builder strings.Builder
	for _, block := range message.Content {
		switch block.Type {
		case core.ContentTypeText, core.ContentTypeStructured:
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

// ToolResultBlocks returns the tool result blocks of a message.
func ToolResultBlocks(message core.Message) []core.ContentBlock {
	results := make([]core.ContentBlock, 0, len(message.Content))
	for _, block := range message.Content {
		if block.Type == core.ContentTypeToolResult {
			results = append(results, block)
		}
	}
	return results
}

// IsJSONObject reports whether raw is a non-null JSON object.
func IsJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

// CloneRawJSON copies a JSON payload so callers cannot mutate stored state.
func CloneRawJSON(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// CloneMessages deep copies a message history.
func CloneMessages(messages []core.Message) []core.Message {
	if messages == nil {
		return nil
	}
	cloned := make([]core.Message, len(messages))
	for index, message := range messages {
		cloned[index] = message
		cloned[index].Content = CloneBlocks(message.Content)
	}
	return cloned
}

// CloneBlocks deep copies content blocks including their JSON payloads.
func CloneBlocks(blocks []core.ContentBlock) []core.ContentBlock {
	if blocks == nil {
		return nil
	}
	cloned := make([]core.ContentBlock, len(blocks))
	for index, block := range blocks {
		cloned[index] = block
		cloned[index].Input = CloneRawJSON(block.Input)
		cloned[index].Output = CloneRawJSON(block.Output)
	}
	return cloned
}

// CloneTools deep copies tool declarations.
func CloneTools(specs []core.ToolSpec) []core.ToolSpec {
	if specs == nil {
		return nil
	}
	cloned := make([]core.ToolSpec, len(specs))
	for index, spec := range specs {
		cloned[index] = spec
		cloned[index].Parameters = CloneRawJSON(spec.Parameters)
	}
	return cloned
}
