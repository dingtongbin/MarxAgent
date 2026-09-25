// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

var (
	globSchema = json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","minLength":1},"path":{"type":"string"},"limit":{"type":"integer","minimum":1}},"required":["pattern"],"additionalProperties":false}`)
	grepSchema = json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","minLength":1},"path":{"type":"string"},"glob":{"type":"string"},"max_matches":{"type":"integer","minimum":1}},"required":["pattern"],"additionalProperties":false}`)
)

type globTool struct {
	workspace *Workspace
}

type grepTool struct {
	workspace *Workspace
}

type globParams struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Limit   int    `json:"limit"`
}

type grepParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	MaxMatches int    `json:"max_matches"`
}

type grepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func NewGlobTool(workspace *Workspace) core.Tool {
	return &globTool{workspace: workspace}
}

func NewGrepTool(workspace *Workspace) core.Tool {
	return &grepTool{workspace: workspace}
}

func (w *Workspace) SearchTools() []core.Tool {
	return []core.Tool{NewGlobTool(w), NewGrepTool(w)}
}

func (t *globTool) Name() string {
	return "glob"
}

func (t *globTool) Description() string {
	return "Find files in the workspace using slash-separated glob patterns."
}

func (t *globTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), globSchema...)
}

func (t *globTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	if t == nil || t.workspace == nil {
		return core.ToolResult{}, fmt.Errorf("tools: glob tool is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return core.ToolResult{}, err
	}
	var params globParams
	if err := decodeParams(raw, &params); err != nil {
		return core.ToolResult{}, err
	}
	if strings.TrimSpace(params.Pattern) == "" {
		return core.ToolResult{}, fmt.Errorf("tools: glob pattern must not be empty")
	}
	pattern := filepath.ToSlash(filepath.Clean(filepath.FromSlash(params.Pattern)))
	if isRootedPattern(params.Pattern) || pattern == ".." || strings.HasPrefix(pattern, "../") {
		return core.ToolResult{}, fmt.Errorf("tools: glob pattern must be relative")
	}
	if err := validateGlobPattern(pattern); err != nil {
		return core.ToolResult{}, err
	}
	base, err := t.workspace.resolveDirectory(defaultString(params.Path, "."))
	if err != nil {
		return core.ToolResult{}, err
	}
	limit := params.Limit
	if limit <= 0 || limit > t.workspace.maxResults {
		limit = t.workspace.maxResults
	}
	files := make([]string, 0)
	walkErr := filepath.WalkDir(base, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		if current == base {
			return nil
		}
		if entry.IsDir() {
			if entry.Type()&fs.ModeSymlink != 0 {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(base, current)
		if err != nil {
			return err
		}
		if !matchGlob(pattern, filepath.ToSlash(relative)) {
			return nil
		}
		files = append(files, t.workspace.relative(current))
		if len(files) > limit {
			return fs.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return core.ToolResult{}, walkErr
	}
	truncated := len(files) > limit
	if truncated {
		files = files[:limit]
	}
	sort.Strings(files)
	output, err := json.Marshal(struct {
		Files     []string `json:"files"`
		Truncated bool     `json:"truncated"`
	}{Files: files, Truncated: truncated})
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Output: output}, nil
}

func (t *grepTool) Name() string {
	return "grep"
}

func (t *grepTool) Description() string {
	return "Search workspace file contents with a regular expression."
}

func (t *grepTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), grepSchema...)
}

func (t *grepTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	if t == nil || t.workspace == nil {
		return core.ToolResult{}, fmt.Errorf("tools: grep tool is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return core.ToolResult{}, err
	}
	var params grepParams
	if err := decodeParams(raw, &params); err != nil {
		return core.ToolResult{}, err
	}
	expression, err := regexp.Compile(params.Pattern)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("tools: invalid grep pattern: %w", err)
	}
	base, err := t.workspace.resolveDirectory(defaultString(params.Path, "."))
	if err != nil {
		return core.ToolResult{}, err
	}
	pattern := filepath.ToSlash(filepath.Clean(filepath.FromSlash(defaultString(params.Glob, "**"))))
	if isRootedPattern(params.Glob) || pattern == ".." || strings.HasPrefix(pattern, "../") {
		return core.ToolResult{}, fmt.Errorf("tools: grep glob must be relative")
	}
	if err := validateGlobPattern(pattern); err != nil {
		return core.ToolResult{}, err
	}
	maxMatches := params.MaxMatches
	if maxMatches <= 0 || maxMatches > t.workspace.maxResults {
		maxMatches = t.workspace.maxResults
	}
	matches := make([]grepMatch, 0)
	walkErr := filepath.WalkDir(base, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		if current == base {
			return nil
		}
		if entry.IsDir() {
			if entry.Type()&fs.ModeSymlink != 0 {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(base, current)
		if err != nil {
			return err
		}
		if !matchGlob(pattern, filepath.ToSlash(relative)) {
			return nil
		}
		data, err := t.workspace.readFile(ctx, current)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		for index, line := range strings.Split(string(data), "\n") {
			if !expression.MatchString(line) {
				continue
			}
			matches = append(matches, grepMatch{Path: t.workspace.relative(current), Line: index + 1, Text: line})
			if len(matches) > maxMatches {
				return fs.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return core.ToolResult{}, walkErr
	}
	truncated := len(matches) > maxMatches
	if truncated {
		matches = matches[:maxMatches]
	}
	sort.SliceStable(matches, func(left, right int) bool {
		if matches[left].Path != matches[right].Path {
			return matches[left].Path < matches[right].Path
		}
		return matches[left].Line < matches[right].Line
	})
	output, err := json.Marshal(struct {
		Matches   []grepMatch `json:"matches"`
		Truncated bool        `json:"truncated"`
	}{Matches: matches, Truncated: truncated})
	if err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Output: output}, nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// isRootedPattern reports whether a user supplied pattern is anchored outside
// the workspace. filepath.IsAbs alone is not enough: on Windows "/etc" and
// "\etc" are rooted but not absolute, so they would slip past an IsAbs check.
func isRootedPattern(pattern string) bool {
	return filepath.IsAbs(pattern) || filepath.VolumeName(pattern) != "" ||
		strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, `\`)
}

func validateGlobPattern(pattern string) error {
	for _, part := range strings.Split(pattern, "/") {
		if part == "**" {
			continue
		}
		if _, err := path.Match(part, ""); err != nil {
			return fmt.Errorf("tools: invalid glob pattern: %w", err)
		}
	}
	return nil
}

func matchGlob(pattern, name string) bool {
	return matchGlobParts(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchGlobParts(patternParts, nameParts []string) bool {
	if len(patternParts) == 0 {
		return len(nameParts) == 0
	}
	if patternParts[0] == "**" {
		for index := 0; index <= len(nameParts); index++ {
			if matchGlobParts(patternParts[1:], nameParts[index:]) {
				return true
			}
		}
		return false
	}
	if len(nameParts) == 0 {
		return false
	}
	matched, err := path.Match(patternParts[0], nameParts[0])
	if err != nil || !matched {
		return false
	}
	return matchGlobParts(patternParts[1:], nameParts[1:])
}
