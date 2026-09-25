// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
)

// Tool is an L2-provided executable capability exposed to the L1 loop.
type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Execute(ctx context.Context, params json.RawMessage) (ToolResult, error)
}

// ToolSpec is the provider-neutral declaration of an available tool.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolInvocation is the mutable input to the pre_tool_exec hook chain.
type ToolInvocation struct {
	ToolCallID string          `json:"tool_call_id"`
	ToolName   string          `json:"tool_name"`
	Input      json.RawMessage `json:"input"`
}

// ToolResult is the provider-neutral output of one tool invocation.
type ToolResult struct {
	ToolCallID string          `json:"tool_call_id"`
	ToolName   string          `json:"tool_name"`
	Output     json.RawMessage `json:"output"`
	IsError    bool            `json:"is_error,omitempty"`
	Error      string          `json:"error,omitempty"`
}
