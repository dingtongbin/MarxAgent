// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

type Pool struct {
	mu      sync.RWMutex
	clients map[string]*Client
	names   []string
}

func NewPool(configs []ServerConfig) (*Pool, error) {
	pool := &Pool{clients: make(map[string]*Client, len(configs))}
	for _, config := range configs {
		if err := pool.Add(config); err != nil {
			return nil, err
		}
	}
	return pool, nil
}

func (p *Pool) Add(config ServerConfig) error {
	if p == nil {
		return fmt.Errorf("%w: nil pool", ErrInvalidConfig)
	}
	client, err := NewClient(config)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.clients[config.Name]; exists {
		return fmt.Errorf("mcp: server %q already exists", config.Name)
	}
	p.clients[config.Name] = client
	p.names = append(p.names, config.Name)
	sort.Strings(p.names)
	return nil
}

func (p *Pool) Get(name string) (*Client, bool) {
	if p == nil {
		return nil, false
	}
	p.mu.RLock()
	client, ok := p.clients[name]
	p.mu.RUnlock()
	return client, ok
}

func (p *Pool) Names() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	names := append([]string(nil), p.names...)
	p.mu.RUnlock()
	return names
}

func (p *Pool) Activate(ctx context.Context, names ...string) error {
	if p == nil {
		return fmt.Errorf("%w: nil pool", ErrInvalidConfig)
	}
	if ctx == nil {
		return ErrInvalidConfig
	}
	if len(names) == 0 {
		names = p.Names()
	}
	for _, name := range names {
		client, ok := p.Get(name)
		if !ok {
			return fmt.Errorf("mcp: server %q is not configured", name)
		}
		if err := client.Start(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pool) Tools(ctx context.Context) ([]core.Tool, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidConfig)
	}
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	clients := p.sortedClients()
	result := make([]core.Tool, 0)
	seen := make(map[string]struct{})
	for _, client := range clients {
		tools, err := client.Tools(ctx)
		if err != nil {
			return nil, err
		}
		for _, tool := range tools {
			if _, exists := seen[tool.Name()]; exists {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateTool, tool.Name())
			}
			seen[tool.Name()] = struct{}{}
			result = append(result, tool)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Name() < result[right].Name()
	})
	return result, nil
}

func (p *Pool) RefreshTools(ctx context.Context) ([]core.Tool, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil pool", ErrInvalidConfig)
	}
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	clients := p.sortedClients()
	result := make([]core.Tool, 0)
	seen := make(map[string]struct{})
	for _, client := range clients {
		tools, err := client.RefreshTools(ctx)
		if err != nil {
			return nil, err
		}
		for _, tool := range tools {
			if _, exists := seen[tool.Name()]; exists {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateTool, tool.Name())
			}
			seen[tool.Name()] = struct{}{}
			result = append(result, tool)
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Name() < result[right].Name()
	})
	return result, nil
}

func (p *Pool) Health(ctx context.Context, names ...string) error {
	if p == nil {
		return fmt.Errorf("%w: nil pool", ErrInvalidConfig)
	}
	if ctx == nil {
		return ErrInvalidConfig
	}
	if len(names) == 0 {
		for _, client := range p.sortedClients() {
			if err := client.Health(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	for _, name := range names {
		client, ok := p.Get(name)
		if !ok {
			return fmt.Errorf("mcp: server %q is not configured", name)
		}
		if err := client.Health(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pool) Close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		return ErrInvalidConfig
	}
	var closeErrors []error
	for _, client := range p.sortedClients() {
		if err := client.Close(ctx); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

func (p *Pool) sortedClients() []*Client {
	p.mu.RLock()
	clients := make([]*Client, 0, len(p.clients))
	for _, name := range p.names {
		clients = append(clients, p.clients[name])
	}
	p.mu.RUnlock()
	return clients
}
