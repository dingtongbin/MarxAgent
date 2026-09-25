// SPDX-License-Identifier: Apache-2.0

package core

import (
	"encoding/json"
	"time"
)

// Role identifies the author of a message.
type Role string

const (
	// RoleSystem identifies system instructions.
	RoleSystem Role = "system"
	// RoleUser identifies user input.
	RoleUser Role = "user"
	// RoleAssistant identifies model output.
	RoleAssistant Role = "assistant"
	// RoleTool identifies a tool result message.
	RoleTool Role = "tool"
)

const (
	// ContentTypeText identifies a text block.
	ContentTypeText = "text"
	// ContentTypeImage identifies a base64 image block.
	ContentTypeImage = "image"
	// ContentTypeToolUse identifies a tool call block.
	ContentTypeToolUse = "tool_use"
	// ContentTypeToolResult identifies a tool result block.
	ContentTypeToolResult = "tool_result"
	// ContentTypeThinking identifies provider-preserved thinking.
	ContentTypeThinking = "thinking"
	// ContentTypeReasoning identifies provider-preserved encrypted reasoning.
	ContentTypeReasoning = "reasoning"
	// ContentTypeStructured identifies JSON user input retained in the universal model.
	ContentTypeStructured = "structured"
)

const (
	// InputTypeText identifies textual user input.
	InputTypeText = "text"
	// InputTypeImage identifies image user input.
	InputTypeImage = "image"
	// InputTypeStructured identifies JSON user input.
	InputTypeStructured = "structured"
)

const (
	// StreamTypeText identifies an incremental text chunk.
	StreamTypeText = "text"
	// StreamTypeToolCall identifies a complete tool call chunk.
	StreamTypeToolCall = "tool_call"
	// StreamTypeThinking identifies an incremental thinking chunk.
	StreamTypeThinking = "thinking"
	// StreamTypeDone identifies normal provider completion.
	StreamTypeDone = "done"
	// StreamTypeError identifies a terminal provider stream error.
	StreamTypeError = "error"
)

// Message is the provider-neutral conversation record.
type Message struct {
	ID         string         `json:"id"`
	Role       Role           `json:"role"`
	Content    []ContentBlock `json:"content"`
	CreatedAt  time.Time      `json:"created_at"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Incomplete bool           `json:"incomplete,omitempty"`
}

// ContentBlock is the provider-neutral content and credential superset.
type ContentBlock struct {
	Type             string          `json:"type"`
	Text             string          `json:"text,omitempty"`
	ImageData        string          `json:"image_data,omitempty"`
	MediaFormat      string          `json:"media_format,omitempty"`
	ImageSource      string          `json:"image_source,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ToolName         string          `json:"tool_name,omitempty"`
	Input            json.RawMessage `json:"input,omitempty"`
	Output           json.RawMessage `json:"output,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	Thinking         string          `json:"thinking,omitempty"`
	ThinkingSig      string          `json:"thinking_sig,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	ResponseID       string          `json:"response_id,omitempty"`
}

// ChatRequest is the provider-neutral model request.
type ChatRequest struct {
	Model       string     `json:"model"`
	Messages    []Message  `json:"messages"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	SystemHint  string     `json:"system_hint,omitempty"`
	MaxTokens   int        `json:"max_tokens,omitempty"`
	Temperature float64    `json:"temperature,omitempty"`
	CacheKey    string     `json:"cache_key,omitempty"`
}

// Input is one user request accepted by Agent.Run.
type Input struct {
	Type     string          `json:"type"`
	Content  string          `json:"content"`
	Data     json.RawMessage `json:"data"`
	Metadata map[string]any  `json:"metadata"`
}

// StreamChunk is one normalized item from a provider stream.
type StreamChunk struct {
	Type             string        `json:"type"`
	Text             string        `json:"text,omitempty"`
	ToolCall         *ContentBlock `json:"tool_call,omitempty"`
	ThinkingSig      string        `json:"thinking_sig,omitempty"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
	ResponseID       string        `json:"response_id,omitempty"`
	Err              error         `json:"-"`
}
