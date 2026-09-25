// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	implementationName    = "marxagent-mcp-client"
	implementationVersion = "0.1.0"
	defaultMaxToolPages   = 100
)

var (
	ErrInvalidConfig = errors.New("mcp: invalid configuration")
	ErrNotStarted    = errors.New("mcp: client is not started")
	ErrToolNotFound  = errors.New("mcp: tool not found")
	ErrDuplicateTool = errors.New("mcp: duplicate tool name")
)

type ServerConfig struct {
	Name              string
	Command           string
	Args              []string
	Env               []string
	Dir               string
	TerminateDuration time.Duration
	MaxToolPages      int
	Sandbox           sandbox.Runner
}

type ClientConfig = ServerConfig

type Tool struct {
	client      *Client
	serverName  string
	remoteName  string
	qualified   string
	description string
	schema      json.RawMessage
}

func (t *Tool) Name() string {
	if t == nil {
		return ""
	}
	return t.qualified
}

func (t *Tool) Description() string {
	if t == nil {
		return ""
	}
	if t.description != "" {
		return t.description
	}
	return fmt.Sprintf("MCP tool %q provided by server %q.", t.remoteName, t.serverName)
}

func (t *Tool) Parameters() json.RawMessage {
	if t == nil || t.schema == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return append(json.RawMessage(nil), t.schema...)
}

func (t *Tool) Execute(ctx context.Context, params json.RawMessage) (core.ToolResult, error) {
	if t == nil || t.client == nil {
		return core.ToolResult{}, fmt.Errorf("%w: nil MCP tool", ErrInvalidConfig)
	}
	if err := validateArguments(params); err != nil {
		return core.ToolResult{}, err
	}
	result, err := t.client.callTool(ctx, t.remoteName, params)
	if err != nil {
		return core.ToolResult{}, err
	}
	if result == nil {
		return core.ToolResult{}, fmt.Errorf("mcp: server %q returned a nil tool result", t.serverName)
	}
	output, err := json.Marshal(result)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("mcp: marshal tool result: %w", err)
	}
	toolResult := core.ToolResult{
		ToolName: t.Name(),
		Output:   output,
		IsError:  result != nil && result.IsError,
	}
	if toolResult.IsError {
		toolResult.Error = "MCP tool reported an error"
	}
	return toolResult, nil
}

func QualifiedName(serverName, remoteName string) (string, error) {
	if err := validateName(serverName); err != nil {
		return "", fmt.Errorf("%w: server name: %v", ErrInvalidConfig, err)
	}
	if err := validateName(remoteName); err != nil {
		return "", fmt.Errorf("%w: tool name: %v", ErrInvalidConfig, err)
	}
	return "mcp__" + serverName + "__" + remoteName, nil
}

func NewClient(config ClientConfig) (*Client, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	client := &Client{
		config: ClientConfig{
			Name:              config.Name,
			Command:           config.Command,
			Args:              append([]string(nil), config.Args...),
			Env:               append([]string(nil), config.Env...),
			Dir:               config.Dir,
			TerminateDuration: config.TerminateDuration,
			MaxToolPages:      config.MaxToolPages,
			Sandbox:           config.Sandbox,
		},
		tools: nil,
	}
	client.sdk = mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    implementationName,
		Version: implementationVersion,
	}, nil)
	return client, nil
}

func (c *Client) Name() string {
	if c == nil {
		return ""
	}
	return c.config.Name
}

func (c *Client) Start(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("%w: nil client", ErrInvalidConfig)
	}
	_, err := c.ensureSession(ctx)
	return err
}

func (c *Client) IsStarted() bool {
	if c == nil {
		return false
	}
	c.stateMu.RLock()
	started := c.session != nil
	c.stateMu.RUnlock()
	return started
}

func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		return ErrInvalidConfig
	}
	c.startMu.Lock()
	c.stateMu.Lock()
	session := c.session
	c.session = nil
	c.tools = nil
	c.stateMu.Unlock()
	c.startMu.Unlock()
	if session == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- session.Close()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) Health(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("%w: nil client", ErrInvalidConfig)
	}
	if ctx == nil {
		return ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.stateMu.RLock()
	session := c.session
	c.stateMu.RUnlock()
	if session == nil {
		return ErrNotStarted
	}
	if err := session.Ping(ctx, &mcpsdk.PingParams{}); err != nil {
		c.invalidate(session)
		return err
	}
	return nil
}

func (c *Client) Tools(ctx context.Context) ([]core.Tool, error) {
	tools, err := c.discover(ctx, false)
	if err != nil {
		return nil, err
	}
	result := make([]core.Tool, 0, len(tools))
	for _, tool := range tools {
		result = append(result, tool)
	}
	return result, nil
}

func (c *Client) RefreshTools(ctx context.Context) ([]core.Tool, error) {
	tools, err := c.discover(ctx, true)
	if err != nil {
		return nil, err
	}
	result := make([]core.Tool, 0, len(tools))
	for _, tool := range tools {
		result = append(result, tool)
	}
	return result, nil
}

func (c *Client) Tool(ctx context.Context, name string) (core.Tool, error) {
	tools, err := c.Tools(ctx)
	if err != nil {
		return nil, err
	}
	for _, tool := range tools {
		if tool.Name() == name {
			return tool, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrToolNotFound, name)
}

func (c *Client) ensureSession(ctx context.Context) (*mcpsdk.ClientSession, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.stateMu.RLock()
	session := c.session
	c.stateMu.RUnlock()
	if session != nil {
		return session, nil
	}
	var connectErr error
	if c.connectFn != nil {
		session, connectErr = c.connectFn(ctx)
	} else if c.config.Sandbox == nil {
		connectErr = sandbox.ErrRequired
	} else {
		transport := &sandboxTransport{runner: c.config.Sandbox, config: c.config}
		session, connectErr = c.sdk.Connect(ctx, transport, nil)
	}
	if connectErr != nil {
		return nil, fmt.Errorf("mcp: connect server %q: %w", c.config.Name, connectErr)
	}
	c.stateMu.Lock()
	c.session = session
	c.tools = nil
	c.stateMu.Unlock()
	return session, nil
}

func (c *Client) discover(ctx context.Context, force bool) ([]*Tool, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.stateMu.RLock()
	session := c.session
	cached := c.tools
	c.stateMu.RUnlock()
	if session == nil {
		var err error
		session, err = c.ensureSessionLocked(ctx)
		if err != nil {
			return nil, err
		}
		cached = nil
	}
	if !force && session != nil && cached != nil {
		return cloneTools(cached), nil
	}
	maxPages := c.config.MaxToolPages
	if maxPages <= 0 {
		maxPages = defaultMaxToolPages
	}
	byName := make(map[string]*Tool)
	cursor := ""
	seenCursors := make(map[string]struct{})
	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			c.invalidate(session)
			return nil, fmt.Errorf("mcp: list tools from %q: %w", c.config.Name, err)
		}
		if result == nil {
			return nil, fmt.Errorf("mcp: server %q returned a nil tools result", c.config.Name)
		}
		for _, remoteTool := range result.Tools {
			if remoteTool == nil {
				return nil, fmt.Errorf("mcp: server %q returned a nil tool", c.config.Name)
			}
			qualified, err := QualifiedName(c.config.Name, remoteTool.Name)
			if err != nil {
				return nil, fmt.Errorf("mcp: server %q: %w", c.config.Name, err)
			}
			if _, exists := byName[qualified]; exists {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateTool, qualified)
			}
			schema, err := marshalSchema(remoteTool.InputSchema)
			if err != nil {
				return nil, fmt.Errorf("mcp: tool %q schema: %w", qualified, err)
			}
			byName[qualified] = &Tool{
				client:      c,
				serverName:  c.config.Name,
				remoteName:  remoteTool.Name,
				qualified:   qualified,
				description: strings.TrimSpace(remoteTool.Description),
				schema:      schema,
			}
		}
		if result.NextCursor == "" {
			break
		}
		if _, exists := seenCursors[result.NextCursor]; exists {
			return nil, fmt.Errorf("mcp: server %q returned a repeated tools cursor %q", c.config.Name, result.NextCursor)
		}
		seenCursors[result.NextCursor] = struct{}{}
		cursor = result.NextCursor
		if page == maxPages-1 {
			return nil, fmt.Errorf("mcp: server %q exceeded tools page limit %d", c.config.Name, maxPages)
		}
	}
	c.stateMu.Lock()
	c.tools = byName
	c.stateMu.Unlock()
	return cloneTools(byName), nil
}

func (c *Client) ensureSessionLocked(ctx context.Context) (*mcpsdk.ClientSession, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.stateMu.RLock()
	session := c.session
	c.stateMu.RUnlock()
	if session != nil {
		return session, nil
	}
	var err error
	if c.connectFn != nil {
		session, err = c.connectFn(ctx)
	} else if c.config.Sandbox == nil {
		err = sandbox.ErrRequired
	} else {
		transport := &sandboxTransport{runner: c.config.Sandbox, config: c.config}
		session, err = c.sdk.Connect(ctx, transport, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: connect server %q: %w", c.config.Name, err)
	}
	c.stateMu.Lock()
	c.session = session
	c.tools = nil
	c.stateMu.Unlock()
	return session, nil
}

func (c *Client) invalidate(session *mcpsdk.ClientSession) {
	c.stateMu.Lock()
	detached := c.session == session
	if detached {
		c.session = nil
		c.tools = nil
	}
	c.stateMu.Unlock()
	if detached {
		go func() {
			_ = session.Close()
		}()
	}
}

func (c *Client) callTool(ctx context.Context, name string, params json.RawMessage) (*mcpsdk.CallToolResult, error) {
	if ctx == nil {
		return nil, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		session, err := c.ensureSession(ctx)
		if err != nil {
			return nil, err
		}
		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      name,
			Arguments: json.RawMessage(append([]byte(nil), params...)),
		})
		if err == nil {
			return result, nil
		}
		c.invalidate(session)
		if attempt == 1 {
			return nil, fmt.Errorf("mcp: call tool %q on %q: %w", name, c.config.Name, err)
		}
	}
	return nil, fmt.Errorf("mcp: call tool %q on %q failed", name, c.config.Name)
}

func validateConfig(config ClientConfig) error {
	if err := validateName(config.Name); err != nil {
		return fmt.Errorf("%w: server name: %v", ErrInvalidConfig, err)
	}
	if strings.TrimSpace(config.Command) == "" {
		return fmt.Errorf("%w: command must not be empty", ErrInvalidConfig)
	}
	if strings.IndexByte(config.Command, 0) >= 0 {
		return fmt.Errorf("%w: command contains NUL", ErrInvalidConfig)
	}
	if strings.IndexByte(config.Dir, 0) >= 0 {
		return fmt.Errorf("%w: directory contains NUL", ErrInvalidConfig)
	}
	for _, argument := range config.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("%w: argument contains NUL", ErrInvalidConfig)
		}
	}
	for _, value := range config.Env {
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("%w: environment contains NUL", ErrInvalidConfig)
		}
	}
	if config.Dir != "" {
		info, err := os.Stat(config.Dir)
		if err != nil {
			return fmt.Errorf("%w: directory: %v", ErrInvalidConfig, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: directory is not a directory", ErrInvalidConfig)
		}
	}
	if config.MaxToolPages < 0 {
		return fmt.Errorf("%w: max tool pages must not be negative", ErrInvalidConfig)
	}
	return nil
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if len(name) > 128 {
		return fmt.Errorf("name is too long")
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' {
			continue
		}
		return fmt.Errorf("name contains invalid character %q", character)
	}
	return nil
}

func validateArguments(params json.RawMessage) error {
	if len(params) == 0 {
		return nil
	}
	if !json.Valid(params) {
		return fmt.Errorf("mcp: tool arguments must be valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(params, &object); err != nil || object == nil {
		return fmt.Errorf("mcp: tool arguments must be a JSON object")
	}
	return nil
}

func marshalSchema(schema any) (json.RawMessage, error) {
	if schema == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`), nil
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || string(data) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`), nil
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("schema is not valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, fmt.Errorf("schema must be a JSON object")
	}
	return data, nil
}

func cloneTools(source map[string]*Tool) []*Tool {
	result := make([]*Tool, 0, len(source))
	for _, tool := range source {
		result = append(result, tool)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].qualified < result[right].qualified
	})
	return result
}

type Client struct {
	config    ClientConfig
	sdk       *mcpsdk.Client
	connectFn func(context.Context) (*mcpsdk.ClientSession, error)

	startMu sync.Mutex
	stateMu sync.RWMutex
	session *mcpsdk.ClientSession
	tools   map[string]*Tool
}
