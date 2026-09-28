// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"time"
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
	// Timeout bounds this one execution, and a hook may set it, because the hook
	// that has seen the call is the one place that knows what the call is worth
	// waiting for: a shell command may legitimately run for minutes and a file
	// read has no business taking a second. Zero or negative means unbounded,
	// which is a caller's own decision rather than a policy this file makes.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// ToolResult is the provider-neutral output of one tool invocation.
type ToolResult struct {
	ToolCallID string          `json:"tool_call_id"`
	ToolName   string          `json:"tool_name"`
	Output     json.RawMessage `json:"output"`
	IsError    bool            `json:"is_error,omitempty"`
	Error      string          `json:"error,omitempty"`
}
