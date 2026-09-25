// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type providerFunc func(ctx context.Context, request ChatRequest) (<-chan StreamChunk, error)

func (function providerFunc) ChatStream(
	ctx context.Context,
	request ChatRequest,
) (<-chan StreamChunk, error) {
	return function(ctx, request)
}

func (providerFunc) Name() string {
	return "test-provider"
}

type testTool struct {
	name        string
	description string
	parameters  json.RawMessage
	execute     func(ctx context.Context, params json.RawMessage) (ToolResult, error)
}

func (tool testTool) Name() string {
	return tool.name
}

func (tool testTool) Description() string {
	return tool.description
}

func (tool testTool) Parameters() json.RawMessage {
	return tool.parameters
}

func (tool testTool) Execute(
	ctx context.Context,
	params json.RawMessage,
) (ToolResult, error) {
	return tool.execute(ctx, params)
}

type recordingProvider struct {
	mu       sync.Mutex
	requests []ChatRequest
	streams  [][]StreamChunk
	errors   []error
	calls    int
}

func (provider *recordingProvider) Name() string {
	return "recording-provider"
}

func (provider *recordingProvider) ChatStream(
	ctx context.Context,
	request ChatRequest,
) (<-chan StreamChunk, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.requests = append(provider.requests, request)
	callIndex := provider.calls
	provider.calls++
	if callIndex < len(provider.errors) && provider.errors[callIndex] != nil {
		return nil, provider.errors[callIndex]
	}
	if callIndex >= len(provider.streams) {
		closed := make(chan StreamChunk)
		close(closed)
		return closed, nil
	}
	stream := make(chan StreamChunk, len(provider.streams[callIndex]))
	for _, chunk := range provider.streams[callIndex] {
		stream <- chunk
	}
	close(stream)
	return stream, nil
}

func (provider *recordingProvider) Requests() []ChatRequest {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]ChatRequest(nil), provider.requests...)
}

func newTestAgent(t testing.TB, provider Provider, config Config) Agent {
	t.Helper()
	if config.Model == "" {
		config.Model = "test-model"
	}
	agent, err := NewAgent(provider, config)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	return agent
}

func textInput(content string) Input {
	return Input{Type: InputTypeText, Content: content, Metadata: map[string]any{}}
}

func collectRunEvents(t testing.TB, events <-chan Event) []Event {
	t.Helper()
	collected := make([]Event, 0)
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				return collected
			}
			collected = append(collected, event)
		case <-timeout.C:
			t.Fatal("timed out collecting agent events")
		}
	}
}

func eventTypes(events []Event) []EventType {
	types := make([]EventType, len(events))
	for index, event := range events {
		types[index] = event.Type
	}
	return types
}

func assertEventTypes(t testing.TB, events []Event, expected []EventType) {
	t.Helper()
	actual := eventTypes(events)
	if len(actual) != len(expected) {
		t.Fatalf("event types = %v, want %v", actual, expected)
	}
	for index := range expected {
		if actual[index] != expected[index] {
			t.Fatalf("event types = %v, want %v", actual, expected)
		}
	}
}

func validTestMessage(id string) Message {
	return Message{
		ID:        id,
		Role:      RoleUser,
		Content:   []ContentBlock{{Type: ContentTypeText, Text: id}},
		CreatedAt: time.Date(2026, time.September, 25, 12, 0, 0, 0, time.UTC),
	}
}
