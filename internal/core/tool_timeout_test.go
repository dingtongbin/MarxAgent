// SPDX-License-Identifier: Apache-2.0

package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// scriptedProvider hands out one scripted turn per ChatStream call. Each turn is
// a list of chunks sent before the stream is closed. The first turn issues a
// tool call, the second one answers with plain text, which is the two calls the
// timeout path needs.
type scriptedProvider struct {
	mu    sync.Mutex
	turns [][]core.StreamChunk
	index int
}

func (p *scriptedProvider) ChatStream(ctx context.Context, request core.ChatRequest) (<-chan core.StreamChunk, error) {
	p.mu.Lock()
	if p.index >= len(p.turns) {
		p.mu.Unlock()
		return nil, context.Canceled
	}
	turn := p.turns[p.index]
	p.index++
	p.mu.Unlock()

	stream := make(chan core.StreamChunk, len(turn)+1)
	for _, chunk := range turn {
		stream <- chunk
	}
	close(stream)
	return stream, nil
}

func (p *scriptedProvider) Name() string { return "scripted" }

type hangingTool struct {
	mu       sync.Mutex
	executed int
}

func (h *hangingTool) Name() string { return "hang" }

func (h *hangingTool) Description() string {
	return "Sleeps forever, ignoring the context, to prove the loop survives a hung tool."
}

func (h *hangingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (h *hangingTool) Execute(ctx context.Context, params json.RawMessage) (core.ToolResult, error) {
	h.mu.Lock()
	h.executed++
	h.mu.Unlock()
	// The context is deliberately ignored: only a deadline the loop itself derives
	// can stop a tool like this.
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	return core.ToolResult{}, ctx.Err()
}

type quickTool struct {
	mu       sync.Mutex
	executed int
}

func (q *quickTool) Name() string { return "quick" }

func (q *quickTool) Description() string {
	return "Returns promptly, to guard the default path where no timeout is set."
}

func (q *quickTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (q *quickTool) Execute(ctx context.Context, params json.RawMessage) (core.ToolResult, error) {
	q.mu.Lock()
	q.executed++
	q.mu.Unlock()
	return core.ToolResult{Output: json.RawMessage(`"done"`)}, nil
}

func collectEvents(t *testing.T, events <-chan core.Event) []core.Event {
	t.Helper()
	collected := make([]core.Event, 0, 16)
	timeout := time.After(10 * time.Second)
	for {
		select {
		case event, open := <-events:
			if !open {
				return collected
			}
			collected = append(collected, event)
		case <-timeout:
			t.Fatal("the event stream did not close within 10s")
		}
	}
}

func toolCallChunk(id string) core.StreamChunk {
	return core.StreamChunk{
		Type: core.StreamTypeToolCall,
		ToolCall: &core.ContentBlock{
			Type:       core.ContentTypeToolUse,
			ToolCallID: id,
			ToolName:   "hang",
			Input:      json.RawMessage(`{}`),
		},
	}
}

func TestToolTimeoutExceededBudgetContinuesTurn(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{turns: [][]core.StreamChunk{
		{
			toolCallChunk("call-1"),
			{Type: core.StreamTypeDone},
		},
		{
			{Type: core.StreamTypeText, Text: "the tool timed out and the turn went on"},
			{Type: core.StreamTypeDone},
		},
	}}
	tool := &hangingTool{}
	agent, err := core.NewAgent(provider, core.Config{
		Model:         "test-model",
		MaxIterations: 4,
		Tools:         []core.Tool{tool},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// The hook sets the budget on the invocation it is handed, which is the
	// documented way for a hook to bound one execution.
	if err := agent.Hooks().Register(core.HookPreToolExec, 50, func(ctx context.Context, data any) (any, error) {
		invocation, ok := data.(core.ToolInvocation)
		if !ok {
			return data, nil
		}
		invocation.Timeout = 50 * time.Millisecond
		return invocation, nil
	}); err != nil {
		t.Fatalf("register hook: %v", err)
	}

	events, err := agent.Run(context.Background(), core.Input{Type: core.InputTypeText, Content: "run the tool"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	collected := collectEvents(t, events)

	// The turn must have gone on past the failed first round: the second provider
	// turn ran and its text is in the state.
	if provider.index < 2 {
		t.Fatalf("the loop stopped after %d provider turns, want the second round to run", provider.index)
	}
	foundResult := false
	foundText := false
	for _, event := range collected {
		if event.Type != core.EventTypeToolResult && event.Type != core.EventTypeStateChange {
			continue
		}
		if strings.Contains(event.Data, "exceeded its 50ms budget") && strings.Contains(event.Data, "deadline") {
			foundResult = true
		}
		if strings.Contains(event.Data, "the tool timed out") {
			foundText = true
		}
	}
	if !foundResult {
		t.Fatalf("no event carries the budget error, events: %s", eventsSummary(collected))
	}
	if !foundText {
		t.Fatalf("the second round text never appeared, events: %s", eventsSummary(collected))
	}

	// The tool result in state must be an error result.
	snapshot := agent.State()
	var resultJSON map[string]any
	for _, message := range snapshot.Messages() {
		for _, block := range message.Content {
			if block.Type != core.ContentTypeToolResult {
				continue
			}
			if err := json.Unmarshal(block.Output, &resultJSON); err != nil {
				t.Fatalf("tool result output is not JSON: %v", err)
			}
			if text, _ := resultJSON["error"].(string); !strings.Contains(text, "budget") || !strings.Contains(text, "deadline") {
				t.Fatalf("tool result error = %q, want the budget wording with the deadline cause", text)
			}
		}
	}
}

func TestToolTimeoutDefaultPathWithoutTimeout(t *testing.T) {
	t.Parallel()

	provider := &scriptedProvider{turns: [][]core.StreamChunk{
		{
			core.StreamChunk{
				Type: core.StreamTypeToolCall,
				ToolCall: &core.ContentBlock{
					Type:       core.ContentTypeToolUse,
					ToolCallID: "call-1",
					ToolName:   "quick",
					Input:      json.RawMessage(`{}`),
				},
			},
			{Type: core.StreamTypeDone},
		},
		{
			{Type: core.StreamTypeText, Text: "all done"},
			{Type: core.StreamTypeDone},
		},
	}}
	tool := &quickTool{}
	agent, err := core.NewAgent(provider, core.Config{
		Model:         "test-model",
		MaxIterations: 4,
		Tools:         []core.Tool{tool},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	// No hook sets a timeout: the default path must be unaffected.
	events, err := agent.Run(context.Background(), core.Input{Type: core.InputTypeText, Content: "run quickly"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	collected := collectEvents(t, events)

	if provider.index != 2 {
		t.Fatalf("provider turns = %d, want 2", provider.index)
	}
	var doneEvent *core.Event
	for index := range collected {
		if collected[index].Type == core.EventTypeDone {
			doneEvent = &collected[index]
		}
	}
	if doneEvent == nil {
		t.Fatalf("no done event among %d events", len(collected))
	}

	snapshot := agent.State()
	quickExecutions := 0
	for _, message := range snapshot.Messages() {
		for _, block := range message.Content {
			if block.Type == core.ContentTypeToolResult && block.ToolName == "quick" {
				quickExecutions++
				if block.IsError {
					t.Fatalf("quick result is an error: %s", block.Output)
				}
			}
		}
	}
	if quickExecutions != 1 {
		t.Fatalf("quick tool results in state = %d, want 1", quickExecutions)
	}
}

// eventsSummary renders events into one line for failure messages.
func eventsSummary(events []core.Event) string {
	parts := make([]string, 0, len(events))
	for _, event := range events {
		text := string(event.Type)
		if event.Error != "" {
			text += "(" + event.Error + ")"
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, ", ")
}
