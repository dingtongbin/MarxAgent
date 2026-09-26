// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func cachePayload(t *testing.T) *anthropicRequest {
	t.Helper()
	request := &core.ChatRequest{
		Model:      "claude-sonnet-4-5",
		SystemHint: "be terse",
		Tools:      []core.ToolSpec{toolSpec("read"), toolSpec("write")},
		Messages: []core.Message{
			textMessage(core.RoleUser, "1", "hi"),
			textMessage(core.RoleAssistant, "2", "hello"),
			textMessage(core.RoleUser, "3", "next"),
		},
	}
	return buildAnthropic(t, request)
}

func TestCacheHintMapperMarksTheStablePrefix(t *testing.T) {
	payload := cachePayload(t)
	if err := (anthropicCacheHintMapper{}).ApplyCacheHints(payload, CacheHints{
		StablePrefix:   true,
		HistoryPrefix:  true,
		MaxBreakpoints: DefaultCacheBreakpoints,
	}); err != nil {
		t.Fatal(err)
	}
	system, ok := payload.System.([]anthropicBlock)
	if !ok || len(system) != 1 || system[0].Text != "be terse" {
		t.Fatalf("system = %#v", payload.System)
	}
	if system[0].CacheControl == nil || system[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("system breakpoint = %#v", system[0].CacheControl)
	}
	if payload.Tools[0].CacheControl == nil {
		t.Fatal("tools breakpoint is missing")
	}
	if payload.Tools[1].CacheControl != nil {
		t.Fatal("only the last tool may carry a breakpoint")
	}
	last := payload.Messages[len(payload.Messages)-1]
	if last.Content[len(last.Content)-1].CacheControl == nil {
		t.Fatalf("history breakpoint is missing: %#v", last)
	}
	if payload.Messages[0].Content[0].CacheControl != nil {
		t.Fatal("an early message carries a breakpoint")
	}
	if got := cacheControlCount(payload); got != 3 {
		t.Fatalf("breakpoints = %d, want 3", got)
	}
}

func TestCacheHintMapperKeepsTheDynamicTailOutsideTheBoundary(t *testing.T) {
	payload := cachePayload(t)
	if err := (anthropicCacheHintMapper{}).ApplyCacheHints(payload, CacheHints{
		HistoryPrefix:  true,
		DynamicTail:    true,
		MaxBreakpoints: DefaultCacheBreakpoints,
	}); err != nil {
		t.Fatal(err)
	}
	newest := payload.Messages[len(payload.Messages)-1]
	if newest.Content[len(newest.Content)-1].CacheControl != nil {
		t.Fatalf("the newest message is cached: %#v", newest)
	}
	previous := payload.Messages[len(payload.Messages)-2]
	if previous.Content[len(previous.Content)-1].CacheControl == nil {
		t.Fatalf("the boundary did not move in front of the dynamic tail: %#v", previous)
	}
	if got := cacheControlCount(payload); got != 1 {
		t.Fatalf("breakpoints = %d, want 1", got)
	}
	// Without a dynamic tail the boundary returns to the newest message.
	plain := cachePayload(t)
	if err := (anthropicCacheHintMapper{}).ApplyCacheHints(plain, CacheHints{
		HistoryPrefix: true, MaxBreakpoints: DefaultCacheBreakpoints,
	}); err != nil {
		t.Fatal(err)
	}
	last := plain.Messages[len(plain.Messages)-1]
	if last.Content[len(last.Content)-1].CacheControl == nil {
		t.Fatalf("the newest message is not cached: %#v", last)
	}
}

func TestCacheHintMapperDropsTheLowestValueSlot(t *testing.T) {
	cases := []struct {
		name      string
		hints     CacheHints
		wantPlan  BreakpointPlan
		wantCount int
	}{
		{
			name: "everything fits",
			hints: CacheHints{
				StablePrefix: true, HistoryPrefix: true, DynamicTail: true,
				MaxBreakpoints: DefaultCacheBreakpoints,
			},
			wantPlan:  BreakpointPlan{System: true, Tools: true, History: true, DynamicTail: true},
			wantCount: 3,
		},
		{
			name: "history and dynamic are dropped",
			hints: CacheHints{
				StablePrefix: true, HistoryPrefix: true, DynamicTail: true,
				MaxBreakpoints: 2,
			},
			wantPlan:  BreakpointPlan{System: true, Tools: true, DynamicTail: true, Dropped: 1},
			wantCount: 2,
		},
		{
			name: "only the system survives",
			hints: CacheHints{
				StablePrefix: true, HistoryPrefix: true, DynamicTail: true,
				MaxBreakpoints: 1,
			},
			wantPlan:  BreakpointPlan{System: true, DynamicTail: true, Dropped: 2},
			wantCount: 1,
		},
		{
			name: "tools are dropped before the system",
			hints: CacheHints{
				StablePrefix: true, HistoryPrefix: true, DynamicTail: true,
				MaxBreakpoints: 0,
			},
			wantPlan:  BreakpointPlan{System: true, Tools: true, History: true, DynamicTail: true},
			wantCount: 3,
		},
		{
			name: "a limit above the format maximum is clamped",
			hints: CacheHints{
				StablePrefix: true, HistoryPrefix: true, DynamicTail: true,
				MaxBreakpoints: 99,
			},
			wantPlan:  BreakpointPlan{System: true, Tools: true, History: true, DynamicTail: true},
			wantCount: 3,
		},
		{
			name:      "nothing is wanted",
			hints:     CacheHints{MaxBreakpoints: DefaultCacheBreakpoints},
			wantPlan:  BreakpointPlan{},
			wantCount: 0,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			payload := cachePayload(t)
			if err := (anthropicCacheHintMapper{}).ApplyCacheHints(payload, test.hints); err != nil {
				t.Fatal(err)
			}
			if got := PlanBreakpoints(test.hints); got != test.wantPlan {
				t.Fatalf("plan = %#v, want %#v", got, test.wantPlan)
			}
			if got := cacheControlCount(payload); got != test.wantCount {
				t.Fatalf("breakpoints = %d, want %d", got, test.wantCount)
			}
		})
	}
}

func TestCacheHintMapperAcceptsBlockSystem(t *testing.T) {
	payload := cachePayload(t)
	payload.System = []anthropicBlock{{Type: "text", Text: "be terse"}}
	if err := (anthropicCacheHintMapper{}).ApplyCacheHints(payload, CacheHints{
		StablePrefix: true, MaxBreakpoints: 1,
	}); err != nil {
		t.Fatal(err)
	}
	blocks, ok := payload.System.([]anthropicBlock)
	if !ok || blocks[0].CacheControl == nil {
		t.Fatalf("system = %#v", payload.System)
	}
	if cacheControlCount(payload) != 1 {
		t.Fatalf("breakpoints = %d", cacheControlCount(payload))
	}
}

func TestCacheHintMapperClearsAnExistingBoundary(t *testing.T) {
	payload := cachePayload(t)
	mapper := anthropicCacheHintMapper{}
	if err := mapper.ApplyCacheHints(payload, CacheHints{StablePrefix: true, MaxBreakpoints: 4}); err != nil {
		t.Fatal(err)
	}
	if cacheControlCount(payload) == 0 {
		t.Fatal("no boundary was placed")
	}
	if err := mapper.ApplyCacheHints(payload, CacheHints{MaxBreakpoints: 4}); err != nil {
		t.Fatal(err)
	}
	if cacheControlCount(payload) != 0 {
		t.Fatalf("stale boundaries remain: %d", cacheControlCount(payload))
	}
}

func TestCacheHintMapperRejectsForeignPayloads(t *testing.T) {
	if err := (anthropicCacheHintMapper{}).ApplyCacheHints(map[string]any{}, CacheHints{}); err == nil {
		t.Fatal("foreign payload accepted")
	}
	if err := anthropicAdapterFor(t).ApplyCacheHints(map[string]any{}, CacheHints{}); err == nil {
		t.Fatal("adapter accepted a foreign payload")
	}
}

func TestCacheHintMapperSkipsEmptySections(t *testing.T) {
	payload := &anthropicRequest{Model: "claude-sonnet-4-5", MaxTokens: 16}
	if err := (anthropicCacheHintMapper{}).ApplyCacheHints(payload, CacheHints{
		StablePrefix: true, HistoryPrefix: true, MaxBreakpoints: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if payload.System != nil {
		t.Fatalf("system = %#v", payload.System)
	}
	if cacheControlCount(payload) != 0 {
		t.Fatalf("breakpoints on an empty payload = %d", cacheControlCount(payload))
	}
}

func TestAnthropicCacheStatsNormalizeThePrompt(t *testing.T) {
	stats := anthropicCacheHintMapper{}.ExtractCacheStats(Usage{
		InputTokens:      10,
		CacheReadTokens:  30,
		CacheWriteTokens: 20,
	})
	if stats.ReadTokens != 30 || stats.WriteTokens != 20 {
		t.Fatalf("stats = %#v", stats)
	}
	if stats.InputTokens != 50 {
		t.Fatalf("input tokens = %d, want 50", stats.InputTokens)
	}
	if rate := stats.HitRate(); rate != 0.6 {
		t.Fatalf("hit rate = %v", rate)
	}
	small := anthropicCacheHintMapper{}.ExtractCacheStats(Usage{InputTokens: 100, CacheReadTokens: 10})
	if small.InputTokens != 100 {
		t.Fatalf("input tokens = %d", small.InputTokens)
	}
}

func TestCompletionsAndResponsesCacheStatsAreIdentical(t *testing.T) {
	usage := Usage{InputTokens: 40, CacheReadTokens: 8, CacheWriteTokens: 2}
	completions := completionsAdapterFor(t).ExtractCacheStats(usage)
	responses := responsesAdapterFor(t).ExtractCacheStats(usage)
	if completions != responses {
		t.Fatalf("completions = %#v responses = %#v", completions, responses)
	}
}

func TestGatewayAppliesCacheHintsForEveryFormat(t *testing.T) {
	for _, format := range Formats() {
		if format == testFormat || strings.HasSuffix(string(format), "-test-format") {
			continue
		}
		adapter, err := NewAdapter(format)
		if err != nil {
			t.Fatal(err)
		}
		mapper := mapperFor(format, adapter)
		payload, err := adapter.BuildRequest(&core.ChatRequest{
			Model:      "model",
			SystemHint: "be terse",
			Tools:      []core.ToolSpec{toolSpec("read")},
			Messages:   []core.Message{textMessage(core.RoleUser, "1", "hi")},
		})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if err := mapper.ApplyCacheHints(payload, CacheHints{
			StablePrefix: true, HistoryPrefix: true, MaxBreakpoints: DefaultCacheBreakpoints,
		}); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
	}
}

func TestGatewayRequestCarriesCacheControlOnTheWire(t *testing.T) {
	var received anthropicRequest
	server := newAnthropicServer(t, func(writer *responseRecorder) {
		writer.named("message_stop", `{"type":"message_stop"}`)
	}, &received)
	defer server.Close()
	gateway, err := New(Config{Format: FormatAnthropicMessages, Endpoint: server.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := gateway.ChatStream(t.Context(), core.ChatRequest{
		Model:      "claude-sonnet-4-5",
		SystemHint: "be terse",
		Tools:      []core.ToolSpec{toolSpec("read")},
		Messages:   []core.Message{textMessage(core.RoleUser, "1", "hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	encoded, err := json.Marshal(received)
	if err != nil {
		t.Fatal(err)
	}
	// The system prompt and the tool list carry a boundary; a single-message
	// request has no history worth marking.
	if got := strings.Count(string(encoded), `"cache_control"`); got != 2 {
		t.Fatalf("boundaries on the wire = %d, payload = %s", got, encoded)
	}
}
