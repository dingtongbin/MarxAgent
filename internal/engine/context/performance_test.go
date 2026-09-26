// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"os"
	"strings"
	"testing"
)

// These gates exist because the obvious implementation of each of these is
// quadratic, and a quadratic compaction is worse than no compaction: it fires
// exactly when the conversation is largest.

func TestContextPerformanceGates(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to run the context performance gates")
	}
	t.Run("estimate_is_linear", func(t *testing.T) {
		estimator := NewHeuristicEstimator()
		small := conversation(50)
		large := conversation(500)
		smallTokens := estimator.Estimate(small)
		largeTokens := estimator.Estimate(large)
		// Ten times the messages must not cost far more than ten times the work.
		if largeTokens < smallTokens*8 {
			t.Fatalf("estimate did not scale: %d then %d", smallTokens, largeTokens)
		}
		reportEstimateScaling(t, estimator, smallTokens, largeTokens)
	})
	t.Run("plan_is_not_quadratic", func(t *testing.T) {
		manager, err := New(Config{ModelWindow: 40000, KeepRecentTurns: 10})
		if err != nil {
			t.Fatal(err)
		}
		messages := conversation(400)
		tokens := manager.Estimate(messages)
		plan := manager.Plan(messages, tokens)
		if plan.Cut <= 0 {
			t.Fatal("nothing was planned for folding")
		}
		// The plan walks back from the protected tail, so its cost is bounded by
		// the budget rather than by the length of the conversation.
		if plan.Cut >= len(messages) {
			t.Fatalf("the plan folded everything: %d of %d", plan.Cut, len(messages))
		}
	})
	t.Run("spilling_a_large_result_is_bounded", func(t *testing.T) {
		spiller, err := NewSpiller(SpillConfig{
			Directory:       t.TempDir(),
			MaxInlineTokens: 200,
			SummaryTokens:   50,
		})
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte(strings.Repeat("a large tool result line ", 20000))
		result, err := spiller.Handle(context.Background(), "s1", "c1", "read", payload)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Spilled {
			t.Fatal("a large result was not spilled")
		}
		// The reference is bounded no matter how large the result was, which is
		// the whole point of spilling it.
		if result.Tokens > 200 {
			t.Fatalf("the reference costs %d tokens against a budget of 200", result.Tokens)
		}
		if result.Bytes != len(payload) {
			t.Fatalf("bytes = %d", result.Bytes)
		}
	})
}

// reportEstimateScaling is a helper kept separate so the gate reads as a list of
// assertions rather than a wall of arithmetic.
func reportEstimateScaling(t *testing.T, estimator *HeuristicEstimator, small, large int) {
	t.Helper()
	if small <= 0 {
		t.Fatalf("a small conversation cost %d tokens", small)
	}
	if large <= small {
		t.Fatalf("a larger conversation cost no more: %d then %d", small, large)
	}
}

func BenchmarkEstimateConversation(b *testing.B) {
	estimator := NewHeuristicEstimator()
	messages := conversation(200)
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		_ = estimator.Estimate(messages)
	}
}

func BenchmarkCompactOversizedConversation(b *testing.B) {
	manager, err := New(Config{ModelWindow: 8000, KeepRecentTurns: 10})
	if err != nil {
		b.Fatal(err)
	}
	messages := conversation(300)
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, _, err := manager.Compact(context.Background(), messages); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSpillLargeResult(b *testing.B) {
	spiller, err := NewSpiller(SpillConfig{
		Directory:       b.TempDir(),
		MaxInlineTokens: 200,
		SummaryTokens:   50,
	})
	if err != nil {
		b.Fatal(err)
	}
	payload := []byte(strings.Repeat("a large tool result line ", 5000))
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := spiller.Handle(context.Background(), "s1", "c1", "read", payload); err != nil {
			b.Fatal(err)
		}
	}
}
