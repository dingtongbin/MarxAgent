// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type panickingTool struct{}

func (*panickingTool) Name() string {
	panic("name panic")
}

func (*panickingTool) Description() string {
	return "unused"
}

func (*panickingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{}`)
}

func (*panickingTool) Execute(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{}, nil
}

type blockingErrorContext struct {
	context.Context
	once    sync.Once
	called  chan struct{}
	release chan struct{}
}

func newBlockingErrorContext() *blockingErrorContext {
	return &blockingErrorContext{
		Context: context.Background(),
		called:  make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (ctx *blockingErrorContext) Err() error {
	ctx.once.Do(func() { close(ctx.called) })
	<-ctx.release
	return context.Canceled
}

func TestNewAgentValidatesProviderAndConfiguration(t *testing.T) {
	validProvider := providerFunc(func(context.Context, ChatRequest) (<-chan StreamChunk, error) {
		stream := make(chan StreamChunk)
		close(stream)
		return stream, nil
	})
	var nilProvider providerFunc
	var nilTool *panickingTool
	baseConfig := Config{Model: "model", AgentID: "agent"}
	tests := []struct {
		name     string
		provider Provider
		config   Config
		tools    []Tool
		wantErr  error
	}{
		{name: "nil provider", provider: nil, config: baseConfig, wantErr: ErrNilProvider},
		{name: "typed nil provider", provider: nilProvider, config: baseConfig, wantErr: ErrNilProvider},
		{name: "valid defaults", provider: validProvider, config: Config{Model: "model"}},
		{name: "negative iterations", provider: validProvider, config: Config{Model: "model", MaxIterations: -1}, wantErr: ErrInvalidConfiguration},
		{name: "negative event buffer", provider: validProvider, config: Config{Model: "model", EventBuffer: -1}, wantErr: ErrInvalidConfiguration},
		{name: "large event buffer", provider: validProvider, config: Config{Model: "model", EventBuffer: maximumEventBuffer + 1}, wantErr: ErrInvalidConfiguration},
		{name: "negative max tokens", provider: validProvider, config: Config{Model: "model", MaxTokens: -1}, wantErr: ErrInvalidConfiguration},
		{name: "nan temperature", provider: validProvider, config: Config{Model: "model", Temperature: math.NaN()}, wantErr: ErrInvalidConfiguration},
		{name: "infinite temperature", provider: validProvider, config: Config{Model: "model", Temperature: math.Inf(1)}, wantErr: ErrInvalidConfiguration},
		{name: "empty model", provider: validProvider, config: Config{}, wantErr: ErrInvalidConfiguration},
		{name: "blank agent ID", provider: validProvider, config: Config{Model: "model", AgentID: "  "}, wantErr: ErrInvalidConfiguration},
		{name: "self parent", provider: validProvider, config: Config{Model: "model", AgentID: "agent", ParentID: "agent"}, wantErr: ErrInvalidConfiguration},
		{name: "invalid variables", provider: validProvider, config: Config{Model: "model", Variables: map[string]any{"bad": func() {}}}, wantErr: ErrInvalidConfiguration},
		{name: "invalid metadata", provider: validProvider, config: Config{Model: "model", Metadata: map[string]any{"bad": make(chan int)}}, wantErr: ErrInvalidConfiguration},
		{name: "invalid initial message", provider: validProvider, config: Config{Model: "model", InitialMessages: []Message{{}}}, wantErr: ErrInvalidConfiguration},
		{name: "nil tool", provider: validProvider, config: baseConfig, tools: []Tool{nilTool}, wantErr: ErrInvalidConfiguration},
		{name: "panicking tool", provider: validProvider, config: baseConfig, tools: []Tool{&panickingTool{}}, wantErr: ErrInvalidConfiguration},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := test.config
			config.Tools = test.tools
			agent, err := NewAgent(test.provider, config)
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("NewAgent() error = %v", err)
				}
				if agent == nil {
					t.Fatal("NewAgent() returned nil agent")
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("NewAgent() error = %v, want %v", err, test.wantErr)
			}
			if agent != nil {
				t.Fatal("NewAgent() returned an agent with an error")
			}
		})
	}
}

func TestNewAgentValidatesToolContractsAndSortsSpecs(t *testing.T) {
	execute := func(context.Context, json.RawMessage) (ToolResult, error) {
		return ToolResult{Output: json.RawMessage(`null`)}, nil
	}
	tests := []struct {
		name    string
		tools   []Tool
		wantErr bool
	}{
		{name: "empty name", tools: []Tool{testTool{name: "", description: "desc", parameters: json.RawMessage(`{}`), execute: execute}}, wantErr: true},
		{name: "blank name", tools: []Tool{testTool{name: " ", description: "desc", parameters: json.RawMessage(`{}`), execute: execute}}, wantErr: true},
		{name: "empty description", tools: []Tool{testTool{name: "name", description: "", parameters: json.RawMessage(`{}`), execute: execute}}, wantErr: true},
		{name: "nil parameters", tools: []Tool{testTool{name: "name", description: "desc", execute: execute}}, wantErr: true},
		{name: "invalid parameters", tools: []Tool{testTool{name: "name", description: "desc", parameters: json.RawMessage(`[]`), execute: execute}}, wantErr: true},
		{name: "duplicate", tools: []Tool{
			testTool{name: "same", description: "desc", parameters: json.RawMessage(`{}`), execute: execute},
			testTool{name: "same", description: "desc", parameters: json.RawMessage(`{}`), execute: execute},
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewAgent(emptyStreamProvider(), Config{Model: "model", Tools: test.tools})
			if (err != nil) != test.wantErr {
				t.Fatalf("NewAgent() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}

	alphaParameters := json.RawMessage(`{"type":"object"}`)
	alpha := testTool{name: "alpha", description: "Alpha", parameters: alphaParameters, execute: execute}
	zeta := testTool{name: "zeta", description: "Zeta", parameters: json.RawMessage(`{"type":"object"}`), execute: execute}
	agent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model", Tools: []Tool{zeta, alpha}}).(*agent)
	if got := []string{agent.toolSpecs[0].Name, agent.toolSpecs[1].Name}; !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("tool spec order = %v", got)
	}
	alphaParameters[2] = 'X'
	if string(agent.toolSpecs[0].Parameters) != `{"type":"object"}` {
		t.Fatal("tool spec parameters were not cloned")
	}
}

func TestValidateInputMessageRoleAndJSONHelpers(t *testing.T) {
	validMessage := validTestMessage("id")
	inputTests := []struct {
		name    string
		input   Input
		wantErr bool
	}{
		{name: "text", input: textInput("hello")},
		{name: "empty text", input: Input{Type: InputTypeText}, wantErr: true},
		{name: "image", input: Input{Type: InputTypeImage, Content: "base64", Metadata: map[string]any{"image_source": "camera", "media_format": "image/png"}}},
		{name: "empty image", input: Input{Type: InputTypeImage}, wantErr: true},
		{name: "bad image source", input: Input{Type: InputTypeImage, Content: "base64", Metadata: map[string]any{"image_source": 1}}, wantErr: true},
		{name: "bad media format", input: Input{Type: InputTypeImage, Content: "base64", Metadata: map[string]any{"media_format": true}}, wantErr: true},
		{name: "structured", input: Input{Type: InputTypeStructured, Data: json.RawMessage(`{"ok":true}`)}},
		{name: "structured without data", input: Input{Type: InputTypeStructured}, wantErr: true},
		{name: "invalid structured data", input: Input{Type: InputTypeStructured, Data: json.RawMessage(`{`)}, wantErr: true},
		{name: "unknown", input: Input{Type: "audio"}, wantErr: true},
		{name: "invalid metadata", input: Input{Type: InputTypeText, Content: "x", Metadata: map[string]any{"bad": func() {}}}, wantErr: true},
	}
	for _, test := range inputTests {
		t.Run("input "+test.name, func(t *testing.T) {
			err := validateInput(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateInput() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}

	invalidMessages := []Message{
		{},
		func() Message { message := validMessage; message.ID = " "; return message }(),
		func() Message { message := validMessage; message.Role = "bad"; return message }(),
		func() Message { message := validMessage; message.CreatedAt = time.Time{}; return message }(),
		func() Message { message := validMessage; message.Content = nil; return message }(),
		func() Message {
			message := validMessage
			message.Metadata = map[string]any{"bad": func() {}}
			return message
		}(),
		func() Message {
			message := validMessage
			message.Content = []ContentBlock{{Type: "bad"}}
			return message
		}(),
	}
	for index, message := range invalidMessages {
		t.Run(fmt.Sprintf("message %d", index), func(t *testing.T) {
			if err := validateMessage(message); err == nil {
				t.Fatal("validateMessage() error = nil")
			}
		})
	}
	if err := validateMessage(validMessage); err != nil {
		t.Fatalf("validateMessage(valid) error = %v", err)
	}

	blockTests := []struct {
		block   ContentBlock
		wantErr bool
	}{
		{block: ContentBlock{Type: ContentTypeText}},
		{block: ContentBlock{Type: ContentTypeStructured, Input: json.RawMessage(`{}`)}},
		{block: ContentBlock{Type: ContentTypeStructured}, wantErr: true},
		{block: ContentBlock{Type: ContentTypeImage}, wantErr: true},
		{block: ContentBlock{Type: ContentTypeImage, ImageData: "data"}},
		{block: ContentBlock{Type: ContentTypeToolUse}, wantErr: true},
		{block: ContentBlock{Type: ContentTypeToolUse, ToolCallID: "id", ToolName: "tool"}},
		{block: ContentBlock{Type: ContentTypeToolUse, ToolCallID: "id", ToolName: "tool", Input: json.RawMessage(`[]`)}, wantErr: true},
		{block: ContentBlock{Type: ContentTypeToolResult}, wantErr: true},
		{block: ContentBlock{Type: ContentTypeToolResult, ToolCallID: "id"}},
		{block: ContentBlock{Type: ContentTypeToolResult, ToolCallID: "id", Output: json.RawMessage(`{`)}, wantErr: true},
		{block: ContentBlock{Type: ContentTypeThinking}, wantErr: true},
		{block: ContentBlock{Type: ContentTypeThinking, Thinking: "thought"}},
		{block: ContentBlock{Type: ContentTypeReasoning}, wantErr: true},
		{block: ContentBlock{Type: ContentTypeReasoning, EncryptedContent: "encrypted"}},
		{block: ContentBlock{Type: "unknown"}, wantErr: true},
	}
	for index, test := range blockTests {
		t.Run(fmt.Sprintf("block %d", index), func(t *testing.T) {
			err := validateContentBlock(test.block)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateContentBlock() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}

	for _, role := range []Role{RoleSystem, RoleUser, RoleAssistant, RoleTool, "other"} {
		want := role == RoleSystem || role == RoleUser || role == RoleAssistant || role == RoleTool
		if got := validRole(role); got != want {
			t.Errorf("validRole(%q) = %v, want %v", role, got, want)
		}
	}
	if !isJSONObject(json.RawMessage(` { } `)) || isJSONObject(json.RawMessage(`[]`)) || isJSONObject(nil) || isJSONObject(json.RawMessage(`{`)) {
		t.Fatal("isJSONObject returned an unexpected result")
	}
	if !isNilInterface(nil) || !isNilInterface((*panickingTool)(nil)) || isNilInterface("value") {
		t.Fatal("isNilInterface returned an unexpected result")
	}
}

func TestAgentPureTextPreservesCredentialsAndEventSequence(t *testing.T) {
	provider := &recordingProvider{streams: [][]StreamChunk{{
		{Type: StreamTypeThinking, Text: "reason", ThinkingSig: "sig", ResponseID: "resp-1"},
		{Type: StreamTypeDone, EncryptedContent: "encrypted"},
		{Type: StreamTypeText, Text: "Hello"},
		{Type: StreamTypeText, Text: " world"},
		{Type: StreamTypeDone},
	}}}
	execute := func(context.Context, json.RawMessage) (ToolResult, error) {
		return ToolResult{Output: json.RawMessage(`null`)}, nil
	}
	agent := newTestAgent(t, provider, Config{
		AgentID:     "agent-1",
		SessionID:   "session-1",
		Model:       "model-1",
		SystemHint:  "system",
		MaxTokens:   100,
		Temperature: 0.5,
		Tools: []Tool{
			testTool{name: "zeta", description: "Zeta", parameters: json.RawMessage(`{}`), execute: execute},
			testTool{name: "alpha", description: "Alpha", parameters: json.RawMessage(`{}`), execute: execute},
		},
	})
	eventsChannel, err := agent.Run(context.Background(), textInput("question"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	events := collectRunEvents(t, eventsChannel)
	assertEventTypes(t, events, []EventType{
		EventTypeStateChange,
		EventTypePartial,
		EventTypePartial,
		EventTypePartial,
		EventTypePartial,
		EventTypePartial,
		EventTypeStream,
		EventTypeStateChange,
		EventTypeDone,
	})
	for _, event := range events {
		if event.SessionID != "session-1" || event.AgentID != "agent-1" {
			t.Fatalf("event identity = %q, %q", event.SessionID, event.AgentID)
		}
	}
	messages := agent.State().Messages()
	if len(messages) != 2 {
		t.Fatalf("state message count = %d, want 2", len(messages))
	}
	assistant := messages[1]
	if assistant.Content[0].Thinking != "reason" || assistant.Content[0].ThinkingSig != "sig" {
		t.Fatalf("thinking credentials = %#v", assistant.Content[0])
	}
	if assistant.Content[1].EncryptedContent != "encrypted" {
		t.Fatalf("encrypted reasoning = %#v", assistant.Content[1])
	}
	if assistant.Content[2].Text != "Hello world" {
		t.Fatalf("assistant text = %q", assistant.Content[2].Text)
	}
	if assistant.Metadata["response_id"] != "resp-1" {
		t.Fatalf("response ID metadata = %#v", assistant.Metadata)
	}
	requests := provider.Requests()
	if len(requests) != 1 || requests[0].Model != "model-1" || len(requests[0].Messages) != 1 {
		t.Fatalf("provider request = %#v", requests)
	}
	if requests[0].Tools[0].Name != "alpha" || requests[0].Tools[1].Name != "zeta" {
		t.Fatalf("request tool order = %#v", requests[0].Tools)
	}
}

func TestAgentToolCallThenText(t *testing.T) {
	provider := &recordingProvider{streams: [][]StreamChunk{
		{
			{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call-1", ToolName: "echo", Input: json.RawMessage(`{"value":1}`)}},
			{Type: StreamTypeDone},
		},
		{
			{Type: StreamTypeText, Text: "done"},
			{Type: StreamTypeDone},
		},
	}}
	tool := testTool{
		name:        "echo",
		description: "Echo input",
		parameters:  json.RawMessage(`{"type":"object"}`),
		execute: func(_ context.Context, params json.RawMessage) (ToolResult, error) {
			return ToolResult{Output: append(json.RawMessage(nil), params...)}, nil
		},
	}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	eventsChannel, err := agent.Run(context.Background(), textInput("use tool"))
	if err != nil {
		t.Fatal(err)
	}
	events := collectRunEvents(t, eventsChannel)
	assertEventTypes(t, events, []EventType{
		EventTypeStateChange,
		EventTypePartial,
		EventTypePartial,
		EventTypeStream,
		EventTypeStateChange,
		EventTypeToolCall,
		EventTypeToolResult,
		EventTypeStateChange,
		EventTypePartial,
		EventTypePartial,
		EventTypeStream,
		EventTypeStateChange,
		EventTypeDone,
	})
	messages := agent.State().Messages()
	if len(messages) != 4 {
		t.Fatalf("message count = %d, want 4", len(messages))
	}
	if messages[2].Role != RoleTool || string(messages[2].Content[0].Output) != `{"value":1}` {
		t.Fatalf("tool message = %#v", messages[2])
	}
	if messages[3].Role != RoleAssistant || messages[3].Content[0].Text != "done" {
		t.Fatalf("final assistant = %#v", messages[3])
	}
	for _, event := range events {
		if event.Type == EventTypeToolResult {
			var payload ToolResultPayload
			if err := json.Unmarshal(event.DataBytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Results) != 1 || payload.Results[0].ToolCallID != "call-1" {
				t.Fatalf("tool result payload = %#v", payload)
			}
		}
	}
}

func TestAgentExecutesMultipleToolsConcurrentlyAndPreservesOrder(t *testing.T) {
	provider := &recordingProvider{streams: [][]StreamChunk{
		{
			{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call-1", ToolName: "parallel", Input: json.RawMessage(`{"value":1}`)}},
			{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call-2", ToolName: "parallel", Input: json.RawMessage(`{"value":2}`)}},
			{Type: StreamTypeDone},
		},
		{{Type: StreamTypeText, Text: "finished"}, {Type: StreamTypeDone}},
	}}
	started := make(chan struct{})
	release := make(chan struct{})
	var current atomic.Int32
	var maximum atomic.Int32
	var startOnce sync.Once
	tool := testTool{
		name:        "parallel",
		description: "Parallel tool",
		parameters:  json.RawMessage(`{}`),
		execute: func(_ context.Context, params json.RawMessage) (ToolResult, error) {
			running := current.Add(1)
			defer current.Add(-1)
			for {
				observed := maximum.Load()
				if running <= observed || maximum.CompareAndSwap(observed, running) {
					break
				}
			}
			if running == 2 {
				startOnce.Do(func() { close(started) })
			}
			<-release
			return ToolResult{Output: append(json.RawMessage(nil), params...)}, nil
		},
	}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	eventsChannel, err := agent.Run(context.Background(), textInput("parallel"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("tools did not execute concurrently")
	}
	close(release)
	events := collectRunEvents(t, eventsChannel)
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", maximum.Load())
	}
	var payload ToolResultPayload
	for _, event := range events {
		if event.Type == EventTypeToolResult {
			if err := json.Unmarshal(event.DataBytes(), &payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(payload.Results) != 2 || payload.Results[0].ToolCallID != "call-1" || payload.Results[1].ToolCallID != "call-2" {
		t.Fatalf("parallel results are out of order: %#v", payload.Results)
	}
}

func TestAgentStreamFailuresPreservePartialState(t *testing.T) {
	tests := []struct {
		name              string
		provider          Provider
		wantError         string
		wantIncomplete    bool
		wantAssistantText string
	}{
		{
			name:      "provider error",
			provider:  &recordingProvider{errors: []error{errors.New("provider failed")}},
			wantError: "provider failed",
		},
		{
			name: "nil stream",
			provider: providerFunc(func(context.Context, ChatRequest) (<-chan StreamChunk, error) {
				return nil, nil
			}),
			wantError: "nil stream",
		},
		{
			name: "stream error after text",
			provider: &recordingProvider{streams: [][]StreamChunk{{
				{Type: StreamTypeText, Text: "partial"},
				{Type: StreamTypeError, Text: "stream failed"},
			}}},
			wantError:         "stream failed",
			wantIncomplete:    true,
			wantAssistantText: "partial",
		},
		{
			name: "chunk error",
			provider: &recordingProvider{streams: [][]StreamChunk{{
				{Err: errors.New("chunk failed")},
			}}},
			wantError: "chunk failed",
		},
		{
			name: "unspecified stream error",
			provider: &recordingProvider{streams: [][]StreamChunk{{
				{Type: StreamTypeError},
			}}},
			wantError: "unspecified error",
		},
		{
			name: "unknown chunk",
			provider: &recordingProvider{streams: [][]StreamChunk{{
				{Type: "unknown"},
			}}},
			wantError: "unsupported stream chunk type",
		},
		{
			name: "missing tool call",
			provider: &recordingProvider{streams: [][]StreamChunk{{
				{Type: StreamTypeToolCall},
			}}},
			wantError: "has no tool call",
		},
		{
			name: "duplicate tool call",
			provider: &recordingProvider{streams: [][]StreamChunk{
				{
					{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "same", ToolName: "tool", Input: json.RawMessage(`{}`)}},
					{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "same", ToolName: "tool", Input: json.RawMessage(`{}`)}},
				},
			}},
			wantError: "duplicate tool call ID",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := newTestAgent(t, test.provider, Config{Model: "model"})
			eventsChannel, err := agent.Run(context.Background(), textInput("input"))
			if err != nil {
				t.Fatal(err)
			}
			events := collectRunEvents(t, eventsChannel)
			if !containsEventType(events, EventTypeError) || containsEventType(events, EventTypeDone) {
				t.Fatalf("events = %v", eventTypes(events))
			}
			if !eventErrorContains(events, test.wantError) {
				t.Fatalf("events do not contain error %q: %v", test.wantError, eventTypes(events))
			}
			messages := agent.State().Messages()
			if test.wantAssistantText != "" {
				if len(messages) != 2 || messages[1].Incomplete != test.wantIncomplete || messages[1].Content[0].Text != test.wantAssistantText {
					t.Fatalf("partial assistant state = %#v", messages)
				}
			}
		})
	}
}

func TestAgentCancellationConcurrencyAndRunValidation(t *testing.T) {
	providerStarted := make(chan struct{})
	provider := providerFunc(func(context.Context, ChatRequest) (<-chan StreamChunk, error) {
		select {
		case <-providerStarted:
		default:
			close(providerStarted)
		}
		return make(chan StreamChunk), nil
	})
	agent := newTestAgent(t, provider, Config{Model: "model"})
	ctx, cancel := context.WithCancel(context.Background())
	eventsChannel, err := agent.Run(ctx, textInput("first"))
	if err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, providerStarted)
	if _, err := agent.Run(context.Background(), textInput("second")); !errors.Is(err, ErrConcurrentRun) {
		t.Fatalf("concurrent Run() error = %v", err)
	}
	cancel()
	events := collectRunEvents(t, eventsChannel)
	if containsEventType(events, EventTypeDone) {
		t.Fatalf("canceled run emitted done: %v", eventTypes(events))
	}
	if _, err := agent.Run(context.Background(), Input{Type: InputTypeText}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("Run(invalid) error = %v", err)
	}
	if _, err := agent.Run(nil, textInput("input")); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Run(nil context) error = %v", err)
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := agent.Run(canceled, textInput("input")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(canceled context) error = %v", err)
	}
}

func TestAgentMaxIterationsAndPostLoopError(t *testing.T) {
	tool := testTool{
		name:        "tool",
		description: "Tool",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`"ok"`)}, nil
		},
	}
	provider := &recordingProvider{streams: [][]StreamChunk{{
		{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "tool", Input: json.RawMessage(`{}`)}},
	}}}
	agent := newTestAgent(t, provider, Config{Model: "model", MaxIterations: 1, Tools: []Tool{tool}})
	eventsChannel, err := agent.Run(context.Background(), textInput("loop"))
	if err != nil {
		t.Fatal(err)
	}
	events := collectRunEvents(t, eventsChannel)
	if !eventErrorContains(events, ErrMaxIterations.Error()) || containsEventType(events, EventTypeDone) {
		t.Fatalf("max iteration events = %v", eventTypes(events))
	}

	postLoopCalls := 0
	postLoopAgent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model"})
	if err := postLoopAgent.Hooks().Register(HookPostLoop, 0, func(_ context.Context, data any) (any, error) {
		postLoopCalls++
		if _, ok := data.(StateView); !ok {
			t.Errorf("post_loop data = %T", data)
		}
		return nil, errors.New("post failed")
	}); err != nil {
		t.Fatal(err)
	}
	eventsChannel, err = postLoopAgent.Run(context.Background(), textInput("input"))
	if err != nil {
		t.Fatal(err)
	}
	events = collectRunEvents(t, eventsChannel)
	if postLoopCalls != 1 || !eventErrorContains(events, "post failed") || containsEventType(events, EventTypeDone) {
		t.Fatalf("post-loop events = %v, calls = %d", eventTypes(events), postLoopCalls)
	}
}

func TestAgentHooksTransformLifecycleAndRemainNonBlocking(t *testing.T) {
	provider := &recordingProvider{streams: [][]StreamChunk{
		{
			{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "tool", Input: json.RawMessage(`{"value":1}`)}},
			{Type: StreamTypeDone},
		},
		{{Type: StreamTypeText, Text: "ok"}, {Type: StreamTypeDone}},
	}}
	tool := testTool{
		name:        "tool",
		description: "Tool",
		parameters:  json.RawMessage(`{}`),
		execute: func(_ context.Context, params json.RawMessage) (ToolResult, error) {
			return ToolResult{Output: append(json.RawMessage(nil), params...)}, nil
		},
	}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	observedDone := make(chan struct{})
	hookFailureEvents := make(chan Event, 1)
	unsubscribeFailureEvents := agent.EventBus().Subscribe(EventTypeError, func(_ context.Context, event Event) {
		hookFailureEvents <- event
	})
	defer unsubscribeFailureEvents()
	var observedOnce sync.Once
	register := func(hookType HookType, hook HookFunc) {
		t.Helper()
		if err := agent.Hooks().Register(hookType, 0, hook); err != nil {
			t.Fatal(err)
		}
	}
	register(HookPreLoop, func(_ context.Context, data any) (any, error) {
		input := data.(Input)
		input.Content = "changed input"
		return input, nil
	})
	register(HookPreModelCall, func(_ context.Context, data any) (any, error) {
		panic("isolated pre-model panic")
	})
	register(HookPreModelCall, func(_ context.Context, data any) (any, error) {
		request := data.(ChatRequest)
		request.CacheKey = "cache"
		request.SystemHint += " + hint"
		return request, nil
	})
	register(HookContextInject, func(_ context.Context, data any) (any, error) {
		request := data.(ChatRequest)
		request.Messages = append(request.Messages, Message{
			ID:        "injected",
			Role:      RoleSystem,
			Content:   []ContentBlock{{Type: ContentTypeText, Text: "injected"}},
			CreatedAt: time.Now().UTC(),
		})
		return request, nil
	})
	register(HookPostModelCall, func(_ context.Context, data any) (any, error) {
		if data.(ChatRequest).Model != "model" {
			t.Error("post_model received wrong request")
		}
		return data, nil
	})
	register(HookPreToolExec, func(_ context.Context, data any) (any, error) {
		invocation := data.(ToolInvocation)
		invocation.Input = json.RawMessage(`{"value":2}`)
		return invocation, nil
	})
	register(HookPostToolExec, func(_ context.Context, data any) (any, error) {
		results := data.([]ToolResult)
		results[0].Output = json.RawMessage(`"post"`)
		return results, nil
	})
	register(HookToolResult, func(_ context.Context, data any) (any, error) {
		result := data.(ToolResult)
		result.Output = json.RawMessage(`"result-hook"`)
		return result, nil
	})
	register(HookOnEvent, func(_ context.Context, data any) (any, error) {
		if data.(Event).Type == EventTypeDone {
			observedOnce.Do(func() { close(observedDone) })
		}
		return nil, nil
	})

	eventsChannel, err := agent.Run(context.Background(), textInput("original"))
	if err != nil {
		t.Fatal(err)
	}
	collectRunEvents(t, eventsChannel)
	waitForSignal(t, observedDone)
	hookFailureEvent := waitForEvent(t, hookFailureEvents)
	if !strings.Contains(hookFailureEvent.Error, "hook panic isolated") {
		t.Fatalf("hook panic event = %#v", hookFailureEvent)
	}
	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("request count = %d", len(requests))
	}
	if requests[0].CacheKey != "cache" || requests[0].SystemHint != " + hint" {
		t.Fatalf("pre-model request = %#v", requests[0])
	}
	if len(requests[0].Messages) != 2 || requests[0].Messages[0].Content[0].Text != "changed input" {
		t.Fatalf("pre-loop/context-inject messages = %#v", requests[0].Messages)
	}
	if string(agent.State().Messages()[2].Content[0].Output) != `"result-hook"` {
		t.Fatalf("tool hooks were not chained: %#v", agent.State().Messages()[2])
	}
}

func TestAgentToolGateErrorPreventsExecution(t *testing.T) {
	var executions atomic.Int32
	tool := testTool{
		name:        "tool",
		description: "Tool",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			executions.Add(1)
			return ToolResult{Output: json.RawMessage(`null`)}, nil
		},
	}
	provider := &recordingProvider{streams: [][]StreamChunk{{
		{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "tool", Input: json.RawMessage(`{}`)}},
	}}}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	if err := agent.Hooks().Register(HookToolCallReceived, -50, func(context.Context, any) (any, error) {
		return nil, errors.New("tool denied")
	}); err != nil {
		t.Fatal(err)
	}
	eventsChannel, err := agent.Run(context.Background(), textInput("deny"))
	if err != nil {
		t.Fatal(err)
	}
	events := collectRunEvents(t, eventsChannel)
	if executions.Load() != 0 || !eventErrorContains(events, "tool denied") {
		t.Fatalf("executions = %d, events = %v", executions.Load(), eventTypes(events))
	}
	if len(agent.State().Messages()) != 2 || agent.State().Messages()[1].Role != RoleAssistant {
		t.Fatalf("gated assistant state missing: %#v", agent.State().Messages())
	}
}

func TestAgentStructuredAndImageInputs(t *testing.T) {
	provider := &recordingProvider{}
	structuredAgent := newTestAgent(t, provider, Config{Model: "model"})
	structuredEvents, err := structuredAgent.Run(context.Background(), Input{
		Type:    InputTypeStructured,
		Content: "payload",
		Data:    json.RawMessage(`{"key":"value"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	collectRunEvents(t, structuredEvents)
	structuredBlock := structuredAgent.State().Messages()[0].Content[0]
	if structuredBlock.Type != ContentTypeStructured || string(structuredBlock.Input) != `{"key":"value"}` {
		t.Fatalf("structured block = %#v", structuredBlock)
	}

	imageAgent := newTestAgent(t, &recordingProvider{}, Config{Model: "model"})
	imageEvents, err := imageAgent.Run(context.Background(), Input{
		Type:    InputTypeImage,
		Content: "base64-data",
		Metadata: map[string]any{
			"image_source": "camera",
			"media_format": "image/png",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	collectRunEvents(t, imageEvents)
	imageBlock := imageAgent.State().Messages()[0].Content[0]
	if imageBlock.ImageData != "base64-data" || imageBlock.ImageSource != "camera" || imageBlock.MediaFormat != "image/png" {
		t.Fatalf("image block = %#v", imageBlock)
	}
}

func TestExecuteToolNormalizesErrorsPanicsAndOutput(t *testing.T) {
	good := testTool{
		name:        "good",
		description: "Good",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{}, nil
		},
	}
	failing := testTool{
		name:        "failing",
		description: "Failing",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{}, errors.New("failed")
		},
	}
	panicking := testTool{
		name:        "panicking",
		description: "Panicking",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			panic("tool panic")
		},
	}
	invalid := testTool{
		name:        "invalid",
		description: "Invalid",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`{`)}, nil
		},
	}
	agent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model", Tools: []Tool{good, failing, panicking, invalid}}).(*agent)
	tests := []struct {
		name       string
		toolName   string
		wantError  bool
		wantOutput string
	}{
		{name: "null output", toolName: "good", wantOutput: "null"},
		{name: "tool error", toolName: "failing", wantError: true},
		{name: "tool panic", toolName: "panicking", wantError: true},
		{name: "invalid output", toolName: "invalid", wantError: true},
		{name: "unknown tool", toolName: "unknown", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := agent.executeTool(context.Background(), ToolInvocation{
				ToolCallID: "call",
				ToolName:   test.toolName,
				Input:      json.RawMessage(`{}`),
			})
			if result.IsError != test.wantError || string(result.Output) != test.wantOutput && test.wantOutput != "" {
				t.Fatalf("executeTool() = %#v", result)
			}
			if result.ToolCallID != "call" || result.ToolName != test.toolName {
				t.Fatalf("executeTool() identity = %#v", result)
			}
		})
	}
}

func TestAgentRejectsInvalidLifecycleHookContracts(t *testing.T) {
	tests := []struct {
		name      string
		hookType  HookType
		hook      HookFunc
		toolCall  bool
		wantError string
	}{
		{name: "pre loop error", hookType: HookPreLoop, hook: func(context.Context, any) (any, error) { return nil, errors.New("pre loop error") }, wantError: "pre loop error"},
		{name: "pre loop wrong type", hookType: HookPreLoop, hook: func(context.Context, any) (any, error) { return "wrong", nil }, wantError: "want Input"},
		{name: "pre loop invalid input", hookType: HookPreLoop, hook: func(context.Context, any) (any, error) { return Input{Type: "invalid"}, nil }, wantError: "pre_loop input"},
		{name: "pre model error", hookType: HookPreModelCall, hook: func(context.Context, any) (any, error) { return nil, errors.New("pre model error") }, wantError: "pre model error"},
		{name: "pre model wrong type", hookType: HookPreModelCall, hook: func(context.Context, any) (any, error) { return "wrong", nil }, wantError: "want ChatRequest"},
		{name: "context error", hookType: HookContextInject, hook: func(context.Context, any) (any, error) { return nil, errors.New("context error") }, wantError: "context error"},
		{name: "context wrong type", hookType: HookContextInject, hook: func(context.Context, any) (any, error) { return "wrong", nil }, wantError: "want ChatRequest"},
		{name: "post model error", hookType: HookPostModelCall, hook: func(context.Context, any) (any, error) { return nil, errors.New("post model error") }, wantError: "post model error"},
		{name: "post model wrong type", hookType: HookPostModelCall, hook: func(context.Context, any) (any, error) { return "wrong", nil }, wantError: "want ChatRequest"},
		{name: "tool gate error", hookType: HookToolCallReceived, hook: func(context.Context, any) (any, error) { return nil, errors.New("gate error") }, toolCall: true, wantError: "gate error"},
		{name: "tool gate wrong type", hookType: HookToolCallReceived, hook: func(context.Context, any) (any, error) { return "wrong", nil }, toolCall: true, wantError: "want []ContentBlock"},
		{name: "tool gate invalid", hookType: HookToolCallReceived, hook: func(context.Context, any) (any, error) { return []ContentBlock{{Type: ContentTypeText}}, nil }, toolCall: true, wantError: "has type"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &recordingProvider{}
			if test.toolCall {
				provider.streams = [][]StreamChunk{{
					{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "missing", Input: json.RawMessage(`{}`)}},
				}}
			}
			agent := newTestAgent(t, provider, Config{Model: "model"})
			if err := agent.Hooks().Register(test.hookType, 0, test.hook); err != nil {
				t.Fatal(err)
			}
			eventsChannel, err := agent.Run(context.Background(), textInput("input"))
			if err != nil {
				t.Fatal(err)
			}
			events := collectRunEvents(t, eventsChannel)
			if !eventErrorContains(events, test.wantError) || containsEventType(events, EventTypeDone) {
				t.Fatalf("events = %v, want error %q", eventTypes(events), test.wantError)
			}
		})
	}
}

func TestAgentRecoversProviderPanicAndDoneEventMarshalError(t *testing.T) {
	panickingProvider := providerFunc(func(context.Context, ChatRequest) (<-chan StreamChunk, error) {
		panic("provider panic")
	})
	panickingAgent := newTestAgent(t, panickingProvider, Config{Model: "model"})
	eventsChannel, err := panickingAgent.Run(context.Background(), textInput("input"))
	if err != nil {
		t.Fatal(err)
	}
	events := collectRunEvents(t, eventsChannel)
	if !eventErrorContains(events, "provider panic") {
		t.Fatalf("provider panic events = %v", eventTypes(events))
	}

	coreAgent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model"}).(*agent)
	coreAgent.state.data.Variables = map[string]any{"invalid": func() {}}
	eventsChannel, err = coreAgent.Run(context.Background(), textInput("input"))
	if err != nil {
		t.Fatal(err)
	}
	events = collectRunEvents(t, eventsChannel)
	if !eventErrorContains(events, "marshal done event") {
		t.Fatalf("done marshal events = %v", eventTypes(events))
	}
}

func TestRunToolsRejectsInvalidHookContracts(t *testing.T) {
	call := ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "missing", Input: json.RawMessage(`{}`)}
	tests := []struct {
		name     string
		hookType HookType
		hook     HookFunc
	}{
		{name: "pre error", hookType: HookPreToolExec, hook: func(context.Context, any) (any, error) { return nil, errors.New("pre tool error") }},
		{name: "pre wrong type", hookType: HookPreToolExec, hook: func(context.Context, any) (any, error) { return "wrong", nil }},
		{name: "pre invalid", hookType: HookPreToolExec, hook: func(context.Context, any) (any, error) { return ToolInvocation{}, nil }},
		{name: "post error", hookType: HookPostToolExec, hook: func(context.Context, any) (any, error) { return nil, errors.New("post tool error") }},
		{name: "post wrong type", hookType: HookPostToolExec, hook: func(context.Context, any) (any, error) { return "wrong", nil }},
		{name: "post wrong count", hookType: HookPostToolExec, hook: func(context.Context, any) (any, error) { return []ToolResult{}, nil }},
		{name: "result error", hookType: HookToolResult, hook: func(context.Context, any) (any, error) { return nil, errors.New("result error") }},
		{name: "result wrong type", hookType: HookToolResult, hook: func(context.Context, any) (any, error) { return "wrong", nil }},
		{name: "result invalid", hookType: HookToolResult, hook: func(context.Context, any) (any, error) { return ToolResult{}, nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model"}).(*agent)
			if err := agent.hooks.Register(test.hookType, 0, test.hook); err != nil {
				t.Fatal(err)
			}
			if _, err := agent.runTools(context.Background(), []ContentBlock{call}); err == nil {
				t.Fatal("runTools() error = nil")
			}
		})
	}
}

func TestMessageAccumulatorValidation(t *testing.T) {
	tests := []struct {
		name    string
		chunks  []StreamChunk
		wantErr string
	}{
		{name: "error chunk", chunks: []StreamChunk{{Err: errors.New("chunk error")}}, wantErr: "chunk error"},
		{name: "invalid call", chunks: []StreamChunk{{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolName: "tool"}}}, wantErr: "invalid tool call"},
		{name: "non object input", chunks: []StreamChunk{{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "id", ToolName: "tool", Input: json.RawMessage(`[]`)}}}, wantErr: "invalid tool call"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accumulator := &messageAccumulator{}
			var err error
			for _, chunk := range test.chunks {
				err = accumulator.add(chunk)
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("add() error = %v, want %q", err, test.wantErr)
			}
		})
	}
	accumulator := &messageAccumulator{}
	if calls := (*messageAccumulator)(nil).toolCalls(); calls != nil {
		t.Fatalf("nil toolCalls() = %#v", calls)
	}
	if _, exists := (*messageAccumulator)(nil).message("id", false); exists {
		t.Fatal("nil accumulator produced a message")
	}
	if _, exists := (&messageAccumulator{}).message("id", false); exists {
		t.Fatal("empty accumulator produced a message")
	}
	_ = accumulator
}

func TestMessageAccumulatorMergesContentAndValidators(t *testing.T) {
	accumulator := &messageAccumulator{}
	for _, chunk := range []StreamChunk{
		{Type: StreamTypeThinking, Text: "one"},
		{Type: StreamTypeThinking, Text: "two", ThinkingSig: "signature"},
		{Type: StreamTypeDone, EncryptedContent: "first"},
		{Type: StreamTypeDone, EncryptedContent: "second"},
		{Type: StreamTypeText, Text: "answer"},
	} {
		if err := accumulator.add(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if len(accumulator.content) != 3 ||
		accumulator.content[0].Thinking != "onetwo" ||
		accumulator.content[0].ThinkingSig != "signature" ||
		accumulator.content[1].EncryptedContent != "second" ||
		accumulator.content[2].Text != "answer" {
		t.Fatalf("merged content = %#v", accumulator.content)
	}
	accumulator.appendOrMerge(ContentBlock{Type: "other", Text: "one"})
	accumulator.appendOrMerge(ContentBlock{Type: "other", Text: "two"})
	if len(accumulator.content) != 4 || accumulator.content[3].Text != "one" {
		t.Fatalf("same non-mergeable type = %#v", accumulator.content)
	}

	if err := validateToolCalls([]ContentBlock{{Type: ContentTypeText}}); err == nil {
		t.Fatal("validateToolCalls accepted a non-tool block")
	}
	if err := validateToolCalls([]ContentBlock{{Type: ContentTypeToolUse}}); err == nil {
		t.Fatal("validateToolCalls accepted an invalid tool call")
	}
	duplicateCall := ContentBlock{Type: ContentTypeToolUse, ToolCallID: "same", ToolName: "tool"}
	if err := validateToolCalls([]ContentBlock{duplicateCall, duplicateCall}); err == nil {
		t.Fatal("validateToolCalls accepted duplicate IDs")
	}
	resultTests := []ToolResult{
		{},
		{ToolCallID: "id", ToolName: "tool"},
		{ToolCallID: "id", ToolName: "tool", Output: json.RawMessage(`{`)},
	}
	for _, result := range resultTests {
		if err := validateToolResult(result); err == nil {
			t.Fatalf("validateToolResult accepted %#v", result)
		}
	}
	if cloneToolSpecs(nil) != nil {
		t.Fatal("cloneToolSpecs(nil) must remain nil")
	}
}

func TestUUIDGenerationFailurePaths(t *testing.T) {
	originalGenerate := generateUUID
	originalRead := readRandomBytes
	defer func() {
		generateUUID = originalGenerate
		readRandomBytes = originalRead
	}()

	generateUUID = func() (string, error) { return "", errors.New("uuid error") }
	if _, err := NewAgent(emptyStreamProvider(), Config{Model: "model"}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("NewAgent() UUID error = %v", err)
	}
	generateUUID = originalGenerate
	readRandomBytes = func([]byte) (int, error) { return 0, errors.New("random error") }
	if _, err := newUUID(); err == nil {
		t.Fatal("newUUID() random error = nil")
	}
}

func TestAgentOutputBackpressureReturnsCancellationAtEachBoundary(t *testing.T) {
	t.Run("user state", func(t *testing.T) {
		preLoopCalled := make(chan struct{})
		agent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model"}).(*agent)
		if err := agent.hooks.Register(HookPreLoop, 0, func(context.Context, any) (any, error) {
			close(preLoopCalled)
			return textInput("input"), nil
		}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- agent.executeLoop(ctx, textInput("input"), make(chan Event)) }()
		waitForSignal(t, preLoopCalled)
		cancel()
		if err := waitForLoopResult(t, result); !errors.Is(err, context.Canceled) {
			t.Fatalf("executeLoop() error = %v", err)
		}
	})

	t.Run("loop context", func(t *testing.T) {
		ctx := newBlockingErrorContext()
		agent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model"}).(*agent)
		events := make(chan Event)
		result := make(chan error, 1)
		go func() { result <- agent.executeLoop(ctx, textInput("input"), events) }()
		waitForEvent(t, events)
		waitForSignal(t, ctx.called)
		close(ctx.release)
		if err := waitForLoopResult(t, result); !errors.Is(err, context.Canceled) {
			t.Fatalf("executeLoop() error = %v", err)
		}
	})

	tool := testTool{
		name:        "tool",
		description: "Tool",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`null`)}, nil
		},
	}
	tests := []struct {
		name            string
		chunks          []StreamChunk
		priorEvents     int
		waitForMessages int
		includeTool     bool
	}{
		{name: "partial", chunks: []StreamChunk{{Type: StreamTypeText, Text: "text"}}, priorEvents: 1},
		{name: "incomplete state", chunks: []StreamChunk{{Type: StreamTypeText, Text: "text"}, {Type: StreamTypeError, Text: "failed"}}, priorEvents: 3, waitForMessages: 2},
		{name: "stream completion", chunks: []StreamChunk{{Type: StreamTypeText, Text: "text"}, {Type: StreamTypeDone}}, priorEvents: 3, waitForMessages: 2},
		{name: "assistant state", chunks: []StreamChunk{{Type: StreamTypeText, Text: "text"}, {Type: StreamTypeDone}}, priorEvents: 4},
		{
			name: "tool call",
			chunks: []StreamChunk{
				{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "tool", Input: json.RawMessage(`{}`)}},
				{Type: StreamTypeDone},
			},
			priorEvents: 5,
			includeTool: true,
		},
		{
			name: "tool result",
			chunks: []StreamChunk{
				{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "tool", Input: json.RawMessage(`{}`)}},
				{Type: StreamTypeDone},
			},
			priorEvents: 6,
			includeTool: true,
		},
		{
			name: "tool state",
			chunks: []StreamChunk{
				{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "tool", Input: json.RawMessage(`{}`)}},
				{Type: StreamTypeDone},
			},
			priorEvents: 7,
			includeTool: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Config{Model: "model"}
			if test.includeTool {
				config.Tools = []Tool{tool}
			}
			agent := newTestAgent(t, &recordingProvider{streams: [][]StreamChunk{test.chunks}}, config).(*agent)
			ctx, cancel := context.WithCancel(context.Background())
			events := make(chan Event)
			result := make(chan error, 1)
			go func() { result <- agent.executeLoop(ctx, textInput("input"), events) }()
			for index := 0; index < test.priorEvents; index++ {
				waitForEvent(t, events)
			}
			if test.waitForMessages > 0 {
				waitForStateMessageCount(t, agent, test.waitForMessages)
			}
			cancel()
			if err := waitForLoopResult(t, result); !errors.Is(err, context.Canceled) {
				t.Fatalf("executeLoop() error = %v", err)
			}
		})
	}
}

func TestAgentPostToolErrorStillPersistsResults(t *testing.T) {
	tool := testTool{
		name:        "tool",
		description: "Tool",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`"ok"`)}, nil
		},
	}
	provider := &recordingProvider{streams: [][]StreamChunk{{
		{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "tool", Input: json.RawMessage(`{}`)}},
		{Type: StreamTypeDone},
	}}}
	agent := newTestAgent(t, provider, Config{Model: "model", Tools: []Tool{tool}})
	if err := agent.Hooks().Register(HookPostToolExec, 0, func(context.Context, any) (any, error) {
		return nil, errors.New("post tool failed")
	}); err != nil {
		t.Fatal(err)
	}
	eventsChannel, err := agent.Run(context.Background(), textInput("input"))
	if err != nil {
		t.Fatal(err)
	}
	events := collectRunEvents(t, eventsChannel)
	if !eventErrorContains(events, "post tool failed") {
		t.Fatalf("events = %v", eventTypes(events))
	}
	messages := agent.State().Messages()
	if len(messages) != 3 || messages[2].Role != RoleTool {
		t.Fatalf("post-tool error lost settled result: %#v", messages)
	}
}

func TestAgentEmitRejectsPayloadAndHonorsCancellation(t *testing.T) {
	agent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model"}).(*agent)
	events := make(chan Event, 1)
	if err := agent.emit(context.Background(), events, EventTypeError, make(chan struct{}), ""); err == nil {
		t.Fatal("emit() accepted an unsupported payload")
	}
	events <- Event{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := agent.emit(ctx, events, EventTypeDone, map[string]string{"ok": "yes"}, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("emit() cancellation error = %v", err)
	}
}

func TestGeneratedMessageIDFailuresAreReported(t *testing.T) {
	originalGenerate := generateUUID
	defer func() { generateUUID = originalGenerate }()

	generateUUID = func() (string, error) { return "", errors.New("user id error") }
	userAgent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model", AgentID: "fixed"})
	userEvents, err := userAgent.Run(context.Background(), textInput("input"))
	if err != nil {
		t.Fatal(err)
	}
	if !eventErrorContains(collectRunEvents(t, userEvents), "user id error") {
		t.Fatal("user message ID failure was not reported")
	}

	assistantIDCalls := 0
	generateUUID = func() (string, error) {
		assistantIDCalls++
		if assistantIDCalls == 1 {
			return "generated-user", nil
		}
		return "", errors.New("assistant id error")
	}
	assistantAgent := newTestAgent(t, emptyStreamProvider(), Config{Model: "model", AgentID: "fixed"})
	assistantEvents, err := assistantAgent.Run(context.Background(), textInput("input"))
	if err != nil {
		t.Fatal(err)
	}
	if !eventErrorContains(collectRunEvents(t, assistantEvents), "assistant id error") {
		t.Fatal("assistant message ID failure was not reported")
	}

	toolResultIDCalls := 0
	generateUUID = func() (string, error) {
		toolResultIDCalls++
		if toolResultIDCalls <= 2 {
			return fmt.Sprintf("generated-%d", toolResultIDCalls), nil
		}
		return "", errors.New("tool result id error")
	}
	tool := testTool{
		name:        "tool",
		description: "Tool",
		parameters:  json.RawMessage(`{}`),
		execute: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{Output: json.RawMessage(`null`)}, nil
		},
	}
	provider := &recordingProvider{streams: [][]StreamChunk{{
		{Type: StreamTypeToolCall, ToolCall: &ContentBlock{Type: ContentTypeToolUse, ToolCallID: "call", ToolName: "tool", Input: json.RawMessage(`{}`)}},
	}}}
	toolAgent := newTestAgent(t, provider, Config{Model: "model", AgentID: "fixed", Tools: []Tool{tool}})
	toolEvents, err := toolAgent.Run(context.Background(), textInput("input"))
	if err != nil {
		t.Fatal(err)
	}
	if !eventErrorContains(collectRunEvents(t, toolEvents), "tool result id error") {
		t.Fatal("tool result message ID failure was not reported")
	}
}

func TestUUIDVersionAndVariant(t *testing.T) {
	value, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(value, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Fatalf("UUID format = %q", value)
	}
	if parts[2][0] != '4' || !strings.Contains("89ab", string(parts[3][0])) {
		t.Fatalf("UUID is not version 4 variant 1: %q", value)
	}
}

func waitForLoopResult(t testing.TB, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for agent loop result")
		return nil
	}
}

func waitForStateMessageCount(t testing.TB, agent *agent, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(agent.State().Messages()) < count && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := len(agent.State().Messages()); got < count {
		t.Fatalf("state message count = %d, want at least %d", got, count)
	}
}

func containsEventType(events []Event, eventType EventType) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func eventErrorContains(events []Event, text string) bool {
	for _, event := range events {
		if event.Type == EventTypeError && strings.Contains(event.Error, text) {
			return true
		}
	}
	return false
}

func emptyStreamProvider() Provider {
	return providerFunc(func(context.Context, ChatRequest) (<-chan StreamChunk, error) {
		stream := make(chan StreamChunk)
		close(stream)
		return stream, nil
	})
}
