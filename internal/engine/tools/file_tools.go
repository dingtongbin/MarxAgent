// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

var (
	readSchema  = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":0}},"required":["path"],"additionalProperties":false}`)
	writeSchema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`)
	editSchema  = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string","minLength":1},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_string","new_string"],"additionalProperties":false}`)
)

type readTool struct {
	workspace *Workspace
}

type writeTool struct {
	workspace *Workspace
}

type editTool struct {
	workspace *Workspace
}

type readParams struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type writeParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type editParams struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func NewReadTool(workspace *Workspace) core.Tool {
	return &readTool{workspace: workspace}
}

func NewWriteTool(workspace *Workspace) core.Tool {
	return &writeTool{workspace: workspace}
}

func NewEditTool(workspace *Workspace) core.Tool {
	return &editTool{workspace: workspace}
}

func (w *Workspace) FileTools() []core.Tool {
	return []core.Tool{NewReadTool(w), NewWriteTool(w), NewEditTool(w)}
}

func (t *readTool) Name() string {
	return "read"
}

func (t *readTool) Description() string {
	return "Read a file from the workspace."
}

func (t *readTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), readSchema...)
}

func (t *readTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	if t == nil || t.workspace == nil {
		return core.ToolResult{}, fmt.Errorf("tools: read tool is not initialized")
	}
	var params readParams
	if err := decodeParams(raw, &params); err != nil {
		return core.ToolResult{}, err
	}
	if err := contextError(ctx); err != nil {
		return core.ToolResult{}, err
	}
	if params.Offset < 0 || params.Limit < 0 {
		return core.ToolResult{}, fmt.Errorf("tools: read offset and limit must not be negative")
	}
	path, err := t.workspace.resolveRead(params.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	data, err := t.workspace.readFile(ctx, path)
	if err != nil {
		return core.ToolResult{}, err
	}
	lines := strings.Split(string(data), "\n")
	if params.Offset > len(lines) {
		params.Offset = len(lines)
	}
	end := len(lines)
	if params.Limit > 0 && params.Limit < end-params.Offset {
		end = params.Offset + params.Limit
	}
	content := strings.Join(lines[params.Offset:end], "\n")
	output, err := json.Marshal(struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		Offset    int    `json:"offset"`
		LineCount int    `json:"line_count"`
		Truncated bool   `json:"truncated"`
	}{
		Path:      t.workspace.relative(path),
		Content:   content,
		Offset:    params.Offset,
		LineCount: end - params.Offset,
		Truncated: end < len(lines),
	})
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Output: output}, nil
}

func (t *writeTool) Name() string {
	return "write"
}

func (t *writeTool) Description() string {
	return "Write UTF-8 text to a file in the workspace."
}

func (t *writeTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), writeSchema...)
}

func (t *writeTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	if t == nil || t.workspace == nil {
		return core.ToolResult{}, fmt.Errorf("tools: write tool is not initialized")
	}
	var params writeParams
	if err := decodeParams(raw, &params); err != nil {
		return core.ToolResult{}, err
	}
	if err := t.workspace.writeFile(ctx, params.Path, []byte(params.Content)); err != nil {
		return core.ToolResult{}, err
	}
	output, err := json.Marshal(struct {
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	}{Path: t.workspace.relative(mustJoin(t.workspace.root, params.Path)), Bytes: len(params.Content)})
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Output: output}, nil
}

func (t *editTool) Name() string {
	return "edit"
}

func (t *editTool) Description() string {
	return "Replace an exact string in a workspace file while preserving its line endings."
}

func (t *editTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), editSchema...)
}

func (t *editTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	if t == nil || t.workspace == nil {
		return core.ToolResult{}, fmt.Errorf("tools: edit tool is not initialized")
	}
	var params editParams
	if err := decodeParams(raw, &params); err != nil {
		return core.ToolResult{}, err
	}
	if err := contextError(ctx); err != nil {
		return core.ToolResult{}, err
	}
	if params.OldString == "" {
		return core.ToolResult{}, fmt.Errorf("tools: old_string must not be empty")
	}
	candidate, err := t.workspace.prepareWrite(params.Path)
	if err != nil {
		return core.ToolResult{}, err
	}
	path, err := t.workspace.resolveRead(candidate)
	if err != nil {
		return core.ToolResult{}, err
	}
	data, err := t.workspace.readFile(ctx, path)
	if err != nil {
		return core.ToolResult{}, err
	}
	oldString := params.OldString
	newString := params.NewString
	if strings.Contains(string(data), "\r\n") {
		oldString = normalizeEditLineEndings(oldString)
		newString = normalizeEditLineEndings(newString)
	}
	count := strings.Count(string(data), oldString)
	if count == 0 || (!params.ReplaceAll && count != 1) {
		return core.ToolResult{}, fmt.Errorf("tools: edit match count is %d", count)
	}
	updated := strings.Replace(string(data), oldString, newString, replacementCount(params.ReplaceAll))
	if int64(len(updated)) > t.workspace.maxFileBytes {
		return core.ToolResult{}, fmt.Errorf("tools: edited content exceeds %d bytes", t.workspace.maxFileBytes)
	}
	if err := t.workspace.writeFile(ctx, path, []byte(updated)); err != nil {
		return core.ToolResult{}, err
	}
	output, err := json.Marshal(struct {
		Path     string `json:"path"`
		Replaced int    `json:"replaced"`
		Bytes    int    `json:"bytes"`
	}{Path: t.workspace.relative(path), Replaced: count, Bytes: len(updated)})
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Output: output}, nil
}

func decodeParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !json.Valid(raw) {
		return fmt.Errorf("tools: parameters must be valid JSON")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("tools: decode parameters: %w", err)
	}
	return nil
}

func replacementCount(all bool) int {
	if all {
		return -1
	}
	return 1
}

func normalizeEditLineEndings(value string) string {
	if strings.Contains(value, "\r\n") {
		return value
	}
	return strings.ReplaceAll(value, "\n", "\r\n")
}

func mustJoin(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}
