// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
)

const (
	// DefaultMaxIterations is the default reasoning and tool-loop limit.
	DefaultMaxIterations = 16
	// DefaultEventBuffer is the default per-run output buffer capacity.
	DefaultEventBuffer = 64
)

const (
	minimumEventBuffer = 1
	maximumEventBuffer = 4096
)

// Agent is the L1 in-memory event-driven loop.
type Agent interface {
	Run(ctx context.Context, input Input) (<-chan Event, error)
	State() StateView
	Hooks() HookRegistry
	EventBus() EventBus
}

// Config defines one agent's immutable assembly parameters and initial state.
type Config struct {
	AgentID         string
	ParentID        string
	SessionID       string
	Model           string
	SystemHint      string
	MaxIterations   int
	MaxTokens       int
	Temperature     float64
	EventBuffer     int
	Tools           []Tool
	InitialMessages []Message
	Variables       map[string]any
	Metadata        map[string]any
}

// ErrNilProvider indicates a missing model provider.
var ErrNilProvider = errors.New("core: provider must not be nil")

// ErrInvalidConfiguration indicates invalid constructor or run configuration.
var ErrInvalidConfiguration = errors.New("core: invalid configuration")

// ErrConcurrentRun indicates a second overlapping run on the same agent.
var ErrConcurrentRun = errors.New("core: agent run already in progress")

// ErrNilContext indicates a nil context passed to Run.
var ErrNilContext = errors.New("core: context must not be nil")

// ErrMaxIterations indicates that the configured loop limit was exhausted.
var ErrMaxIterations = errors.New("core: maximum iterations exhausted")

type agent struct {
	provider  Provider
	config    Config
	state     *state
	hooks     *hookRegistry
	bus       *asyncEventBus
	tools     map[string]Tool
	toolSpecs []ToolSpec
	running   atomic.Bool
}

// NewAgent validates its dependencies and creates an in-memory L1 agent.
func NewAgent(provider Provider, config Config) (Agent, error) {
	if isNilInterface(provider) {
		return nil, ErrNilProvider
	}
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	tools, specs, err := inspectTools(normalized.Tools)
	if err != nil {
		return nil, err
	}
	bus := &asyncEventBus{}
	return &agent{
		provider:  provider,
		config:    normalized,
		state:     newState(normalized),
		hooks:     newHookRegistry(bus, normalized.SessionID, normalized.AgentID),
		bus:       bus,
		tools:     tools,
		toolSpecs: specs,
	}, nil
}

func normalizeConfig(config Config) (Config, error) {
	if config.MaxIterations == 0 {
		config.MaxIterations = DefaultMaxIterations
	}
	if config.MaxIterations < 1 {
		return Config{}, fmt.Errorf("%w: max iterations must be positive", ErrInvalidConfiguration)
	}
	if config.EventBuffer == 0 {
		config.EventBuffer = DefaultEventBuffer
	}
	if config.EventBuffer < minimumEventBuffer || config.EventBuffer > maximumEventBuffer {
		return Config{}, fmt.Errorf("%w: event buffer must be between %d and %d", ErrInvalidConfiguration, minimumEventBuffer, maximumEventBuffer)
	}
	if config.MaxTokens < 0 {
		return Config{}, fmt.Errorf("%w: max tokens must not be negative", ErrInvalidConfiguration)
	}
	if math.IsNaN(config.Temperature) || math.IsInf(config.Temperature, 0) {
		return Config{}, fmt.Errorf("%w: temperature must be finite", ErrInvalidConfiguration)
	}
	if strings.TrimSpace(config.Model) == "" {
		return Config{}, fmt.Errorf("%w: model must not be empty", ErrInvalidConfiguration)
	}
	if config.AgentID == "" {
		agentID, err := generateUUID()
		if err != nil {
			return Config{}, fmt.Errorf("%w: generate agent ID: %v", ErrInvalidConfiguration, err)
		}
		config.AgentID = agentID
	}
	if strings.TrimSpace(config.AgentID) == "" {
		return Config{}, fmt.Errorf("%w: agent ID must not be blank", ErrInvalidConfiguration)
	}
	if config.ParentID == config.AgentID {
		return Config{}, fmt.Errorf("%w: parent ID must differ from agent ID", ErrInvalidConfiguration)
	}
	if config.SessionID == "" {
		config.SessionID = config.AgentID
	}
	if err := validateJSONCompatible(config.Variables); err != nil {
		return Config{}, fmt.Errorf("%w: variables: %v", ErrInvalidConfiguration, err)
	}
	if err := validateJSONCompatible(config.Metadata); err != nil {
		return Config{}, fmt.Errorf("%w: metadata: %v", ErrInvalidConfiguration, err)
	}
	for index, message := range config.InitialMessages {
		if err := validateMessage(message); err != nil {
			return Config{}, fmt.Errorf("%w: initial message %d: %v", ErrInvalidConfiguration, index, err)
		}
	}
	config.InitialMessages = cloneMessages(config.InitialMessages)
	config.Variables = cloneMetadata(config.Variables)
	config.Metadata = cloneMetadata(config.Metadata)
	config.Tools = append([]Tool(nil), config.Tools...)
	return config, nil
}

func inspectTools(configuredTools []Tool) (map[string]Tool, []ToolSpec, error) {
	tools := make(map[string]Tool, len(configuredTools))
	specs := make([]ToolSpec, 0, len(configuredTools))
	for index, tool := range configuredTools {
		if isNilInterface(tool) {
			return nil, nil, fmt.Errorf("%w: tool %d is nil", ErrInvalidConfiguration, index)
		}
		name, description, parameters, err := inspectTool(tool)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: tool %d: %v", ErrInvalidConfiguration, index, err)
		}
		if strings.TrimSpace(name) == "" {
			return nil, nil, fmt.Errorf("%w: tool %d has an empty name", ErrInvalidConfiguration, index)
		}
		if strings.TrimSpace(description) == "" {
			return nil, nil, fmt.Errorf("%w: tool %q has an empty description", ErrInvalidConfiguration, name)
		}
		if !isJSONObject(parameters) {
			return nil, nil, fmt.Errorf("%w: tool %q parameters must be a JSON object", ErrInvalidConfiguration, name)
		}
		if _, exists := tools[name]; exists {
			return nil, nil, fmt.Errorf("%w: duplicate tool name %q", ErrInvalidConfiguration, name)
		}
		tools[name] = tool
		specs = append(specs, ToolSpec{
			Name:        name,
			Description: description,
			Parameters:  cloneRawJSON(parameters),
		})
	}
	sort.Slice(specs, func(left, right int) bool {
		return specs[left].Name < specs[right].Name
	})
	return tools, specs, nil
}

func inspectTool(tool Tool) (name string, description string, parameters json.RawMessage, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("tool inspection panicked: %v", recovered)
		}
	}()
	return tool.Name(), tool.Description(), tool.Parameters(), nil
}

func (a *agent) Run(ctx context.Context, input Input) (<-chan Event, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateInput(input); err != nil {
		return nil, fmt.Errorf("%w: input: %v", ErrInvalidConfiguration, err)
	}
	if !a.running.CompareAndSwap(false, true) {
		return nil, ErrConcurrentRun
	}

	events := make(chan Event, a.config.EventBuffer)
	go a.run(ctx, input, events)
	return events, nil
}

func (a *agent) State() StateView {
	return &stateView{snapshot: a.state.snapshot()}
}

func (a *agent) Hooks() HookRegistry {
	return a.hooks
}

func (a *agent) EventBus() EventBus {
	return a.bus
}

func validateInput(input Input) error {
	switch input.Type {
	case InputTypeText:
		if input.Content == "" {
			return errors.New("text content must not be empty")
		}
	case InputTypeImage:
		if input.Content == "" {
			return errors.New("image content must not be empty")
		}
		if source, exists := input.Metadata["image_source"]; exists {
			if _, ok := source.(string); !ok {
				return errors.New("image_source metadata must be a string")
			}
		}
		if mediaFormat, exists := input.Metadata["media_format"]; exists {
			if _, ok := mediaFormat.(string); !ok {
				return errors.New("media_format metadata must be a string")
			}
		}
	case InputTypeStructured:
		if len(input.Data) == 0 || !json.Valid(input.Data) {
			return errors.New("structured data must be valid JSON")
		}
	default:
		return fmt.Errorf("unsupported input type %q", input.Type)
	}
	return validateJSONCompatible(input.Metadata)
}

func validateMessage(message Message) error {
	if strings.TrimSpace(message.ID) == "" {
		return errors.New("ID must not be empty")
	}
	if !validRole(message.Role) {
		return fmt.Errorf("unsupported role %q", message.Role)
	}
	if message.CreatedAt.IsZero() {
		return errors.New("created_at must not be zero")
	}
	if len(message.Content) == 0 {
		return errors.New("content must not be empty")
	}
	if err := validateJSONCompatible(message.Metadata); err != nil {
		return fmt.Errorf("metadata: %v", err)
	}
	for index, block := range message.Content {
		if err := validateContentBlock(block); err != nil {
			return fmt.Errorf("content block %d: %v", index, err)
		}
	}
	return nil
}

func validateContentBlock(block ContentBlock) error {
	switch block.Type {
	case ContentTypeText:
		return nil
	case ContentTypeStructured:
		if len(block.Input) == 0 || !json.Valid(block.Input) {
			return errors.New("structured input must be valid JSON")
		}
		return nil
	case ContentTypeImage:
		if block.ImageData == "" {
			return errors.New("image_data must not be empty")
		}
		return nil
	case ContentTypeToolUse:
		if block.ToolCallID == "" || block.ToolName == "" {
			return errors.New("tool call ID and name must not be empty")
		}
		if len(block.Input) == 0 {
			return nil
		}
		if !isJSONObject(block.Input) {
			return errors.New("input must be a JSON object")
		}
		return nil
	case ContentTypeToolResult:
		if block.ToolCallID == "" {
			return errors.New("tool result call ID must not be empty")
		}
		if len(block.Output) > 0 && !json.Valid(block.Output) {
			return errors.New("output must be valid JSON")
		}
		return nil
	case ContentTypeThinking:
		if block.Thinking == "" && block.ThinkingSig == "" {
			return errors.New("thinking or signature must not be empty")
		}
		return nil
	case ContentTypeReasoning:
		if block.EncryptedContent == "" {
			return errors.New("encrypted_content must not be empty")
		}
		return nil
	default:
		return fmt.Errorf("unsupported content type %q", block.Type)
	}
}

func validRole(role Role) bool {
	switch role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

func isJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 || !json.Valid(raw) {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ Agent = (*agent)(nil)
