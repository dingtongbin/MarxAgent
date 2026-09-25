// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

type Pool struct {
	mu    sync.RWMutex
	tools map[string]registeredTool
}

type registeredTool struct {
	tool    core.Tool
	enabled bool
}

func NewPool() *Pool {
	return &Pool{tools: make(map[string]registeredTool)}
}

func (p *Pool) Register(tool core.Tool) error {
	return p.register(tool, false)
}

func (p *Pool) RegisterEnabled(tool core.Tool) error {
	return p.register(tool, true)
}

func (p *Pool) RegisterAll(tools []core.Tool) error {
	for _, tool := range tools {
		if err := p.Register(tool); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pool) Unregister(name string) error {
	if p == nil {
		return fmt.Errorf("tools: nil pool")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("tools: tool name must not be empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.tools[name]; !ok {
		return fmt.Errorf("tools: tool %q is not registered", name)
	}
	delete(p.tools, name)
	return nil
}

func (p *Pool) Enable(patterns ...string) error {
	return p.setEnabled(true, patterns)
}

func (p *Pool) Disable(patterns ...string) error {
	return p.setEnabled(false, patterns)
}

func (p *Pool) SetEnabled(name string, enabled bool) error {
	if p == nil {
		return fmt.Errorf("tools: nil pool")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("tools: tool name must not be empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.tools[name]
	if !ok {
		return fmt.Errorf("tools: tool %q is not registered", name)
	}
	entry.enabled = enabled
	p.tools[name] = entry
	return nil
}

func (p *Pool) Get(name string) (core.Tool, bool) {
	if p == nil {
		return nil, false
	}
	p.mu.RLock()
	entry, ok := p.tools[name]
	p.mu.RUnlock()
	if !ok || !entry.enabled {
		return nil, false
	}
	return entry.tool, true
}

func (p *Pool) Lookup(name string) (core.Tool, bool) {
	if p == nil {
		return nil, false
	}
	p.mu.RLock()
	entry, ok := p.tools[name]
	p.mu.RUnlock()
	return entry.tool, ok
}

func (p *Pool) IsEnabled(name string) bool {
	_, ok := p.Get(name)
	return ok
}

func (p *Pool) Tools() []core.Tool {
	return p.filteredTools(true)
}

func (p *Pool) AllTools() []core.Tool {
	return p.filteredTools(false)
}

func (p *Pool) Specs() []core.ToolSpec {
	return specsFor(p.Tools())
}

func (p *Pool) AllSpecs() []core.ToolSpec {
	return specsFor(p.AllTools())
}

func (p *Pool) Execute(ctx context.Context, name string, params json.RawMessage) (core.ToolResult, error) {
	if p == nil {
		return core.ToolResult{}, fmt.Errorf("tools: nil pool")
	}
	if ctx == nil {
		return core.ToolResult{}, fmt.Errorf("tools: context must not be nil")
	}
	tool, ok := p.Get(name)
	if !ok {
		return core.ToolResult{}, fmt.Errorf("tools: enabled tool %q is not registered", name)
	}
	return tool.Execute(ctx, params)
}

func (p *Pool) register(tool core.Tool, enabled bool) error {
	if p == nil {
		return fmt.Errorf("tools: nil pool")
	}
	name, description, parameters, err := inspectTool(tool)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(description) == "" {
		return fmt.Errorf("tools: tool name and description must not be empty")
	}
	if !isJSONObject(parameters) {
		return fmt.Errorf("tools: tool %q parameters must be a JSON object", name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.tools[name]; exists {
		return fmt.Errorf("tools: tool %q is already registered", name)
	}
	p.tools[name] = registeredTool{tool: tool, enabled: enabled}
	return nil
}

func (p *Pool) setEnabled(enabled bool, patterns []string) error {
	if p == nil {
		return fmt.Errorf("tools: nil pool")
	}
	if len(patterns) == 0 {
		return fmt.Errorf("tools: at least one name or pattern is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	selected := make(map[string]struct{})
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return fmt.Errorf("tools: empty name or pattern")
		}
		matched := false
		for name := range p.tools {
			ok, err := path.Match(pattern, name)
			if err != nil {
				return fmt.Errorf("tools: invalid pattern %q: %w", pattern, err)
			}
			if ok {
				selected[name] = struct{}{}
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("tools: pattern %q matched no registered tools", pattern)
		}
	}
	for name := range selected {
		entry := p.tools[name]
		entry.enabled = enabled
		p.tools[name] = entry
	}
	return nil
}

func (p *Pool) filteredTools(enabledOnly bool) []core.Tool {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	result := make([]core.Tool, 0, len(p.tools))
	for _, entry := range p.tools {
		if enabledOnly && !entry.enabled {
			continue
		}
		result = append(result, entry.tool)
	}
	p.mu.RUnlock()
	sort.Slice(result, func(left, right int) bool {
		return result[left].Name() < result[right].Name()
	})
	return result
}

func specsFor(tools []core.Tool) []core.ToolSpec {
	result := make([]core.ToolSpec, 0, len(tools))
	for _, tool := range tools {
		result = append(result, core.ToolSpec{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  append(json.RawMessage(nil), tool.Parameters()...),
		})
	}
	return result
}

func inspectTool(tool core.Tool) (name string, description string, parameters json.RawMessage, err error) {
	if tool == nil {
		return "", "", nil, fmt.Errorf("tools: nil tool")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tools: tool inspection panicked: %v", recovered)
		}
	}()
	return tool.Name(), tool.Description(), tool.Parameters(), nil
}

func isJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}
