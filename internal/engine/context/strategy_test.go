// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// TestSummarizeRegionCarriesEveryBlockKind checks that the text handed to a
// summarizer is actually a description of the conversation. A region described as
// only its prose would lose the tool calls that explain what the prose is about.
func TestSummarizeRegionCarriesEveryBlockKind(t *testing.T) {
	region := []core.Message{
		user("please read the file"),
		{
			Role: core.RoleAssistant,
			Content: []core.ContentBlock{
				{Type: core.ContentTypeText, Text: "reading it now"},
				{Type: core.ContentTypeToolUse, ToolName: "read", Input: []byte(`{"path":"main.go"}`)},
			},
		},
		toolResult("read", "line one\nline two", false),
	}
	var captured string
	strategy := &SummarizeStrategy{
		Summarizer: SummarizerFunc(func(_ context.Context, text string) (string, error) {
			captured = text
			return "a short summary", nil
		}),
	}
	plan := CompactPlan{Cut: len(region), TargetTokens: 0, Estimator: NewHeuristicEstimator()}
	if _, err := strategy.SummarizeRegion(context.Background(), region, plan); err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{
		"user: please read the file",
		"assistant: reading it now",
		"tool_use(read)",
		`{"path":"main.go"}`,
		"tool_result(read)",
		// Newlines inside a result become spaces, so the text handed to a
		// summarizer stays one line per block.
		"line one line two",
	} {
		if !strings.Contains(captured, wanted) {
			t.Fatalf("the summarized text is missing %q:\n%s", wanted, captured)
		}
	}
}

// TestSummarizeRegionCutsAnOversizedSummary covers the guard that stops a summary
// from being larger than the region it replaced, which would make compaction
// pointless while still invalidating the cache.
func TestSummarizeRegionCutsAnOversizedSummary(t *testing.T) {
	long := strings.Repeat("summary text ", 200)
	strategy := &SummarizeStrategy{
		Summarizer: SummarizerFunc(func(context.Context, string) (string, error) {
			return long, nil
		}),
	}
	plan := CompactPlan{
		Cut:          1,
		TargetTokens: 5,
		Estimator:    NewHeuristicEstimator(),
	}
	summary, err := strategy.SummarizeRegion(
		context.Background(), []core.Message{user("x")}, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary) >= len(long) {
		t.Fatalf("the oversized summary was kept whole: %d of %d", len(summary), len(long))
	}
	if len(summary) == 0 {
		t.Fatal("the summary was cut to nothing")
	}
}

// TestSummarizeUsesTheConfiguredHeader proves the wrapper is the caller's to
// choose, because a mode that needs a different instruction in it should not have
// to fork the strategy.
func TestSummarizeUsesTheConfiguredHeader(t *testing.T) {
	strategy := &SummarizeStrategy{
		Header: "<history>\n",
		Summarizer: SummarizerFunc(func(context.Context, string) (string, error) {
			return "what happened", nil
		}),
	}
	compacted, err := strategy.Compact(
		context.Background(),
		CompactPlan{Cut: 1, Estimator: NewHeuristicEstimator()},
		[]core.Message{user("old"), assistant("new")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if compacted[0].Content[0].Text != "<history>\nwhat happened\n</conversation_summary>" {
		t.Fatalf("summary = %q", compacted[0].Content[0].Text)
	}
}

// TestSummarizeRejectsAPlanWithNothingToFold keeps a plan that would produce an
// empty summary from reaching the model.
func TestSummarizeRejectsAPlanWithNothingToFold(t *testing.T) {
	called := false
	strategy := &SummarizeStrategy{
		Summarizer: SummarizerFunc(func(context.Context, string) (string, error) {
			called = true
			return "", nil
		}),
	}
	messages := []core.Message{user("one")}
	for _, cut := range []int{0, -1} {
		compacted, err := strategy.Compact(
			context.Background(), CompactPlan{Cut: cut}, messages)
		if err != nil {
			t.Fatal(err)
		}
		if len(compacted) != len(messages) {
			t.Fatalf("cut %d changed the conversation: %#v", cut, compacted)
		}
	}
	if called {
		t.Fatal("a summarizer was called with nothing to summarize")
	}
}

// TestPreserveKeyResultsHandlesUnidentifiedMessages covers the case where
// messages carry no identifier, so identity falls back to role and first block.
func TestPreserveKeyResultsHandlesUnidentifiedMessages(t *testing.T) {
	manager := newTestManager(t, Config{ModelWindow: 4000, KeepKeyResults: true})
	original := []core.Message{
		toolResult("read", "the exact bytes the model needs", true),
		user("filler one"),
		assistant("filler two"),
		user("filler three"),
		assistant("filler four"),
	}
	plan := CompactPlan{Cut: 3, KeyResults: []int{0}}
	// Nothing was kept from the folded region, so it has to be put back.
	compacted := manager.preserveKeyResults(original, original[3:], plan)
	if len(compacted) != 3 {
		t.Fatalf("compacted = %#v", compacted)
	}
	if !strings.Contains(string(compacted[0].Content[0].Output), "the exact bytes") {
		t.Fatalf("the key result was not restored: %#v", compacted[0])
	}
	// An index outside the original is skipped rather than panicking, because a
	// plan built from a stale estimate is a caller bug that must not take the
	// process down.
	plan = CompactPlan{Cut: 3, KeyResults: []int{-1, 99}}
	compacted = manager.preserveKeyResults(original, original[3:], plan)
	if len(compacted) != 2 {
		t.Fatalf("compacted = %#v", compacted)
	}
	// A key result the strategy already kept is not duplicated.
	plan = CompactPlan{Cut: 1, KeyResults: []int{0}}
	compacted = manager.preserveKeyResults(original, original, plan)
	if len(compacted) != len(original) {
		t.Fatalf("a kept key result was duplicated: %#v", compacted)
	}
}

func TestMessageIdentityPrefersTheIdentifier(t *testing.T) {
	withID := core.Message{
		ID:      "m1",
		Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "text"}},
	}
	if messageIdentity(withID) != "m1" {
		t.Fatalf("identity = %q", messageIdentity(withID))
	}
	withoutID := core.Message{
		Role:    core.RoleUser,
		Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "text"}},
	}
	if !strings.Contains(messageIdentity(withoutID), "user") {
		t.Fatalf("identity = %q", messageIdentity(withoutID))
	}
	// A message with no content at all still has an identity, so a key result
	// carried in an empty message is not lost.
	empty := core.Message{Role: core.RoleTool}
	if messageIdentity(empty) == "" {
		t.Fatal("an empty message had no identity")
	}
}

func TestForceTruncateClampsAnImpossibleCut(t *testing.T) {
	manager := newTestManager(t, Config{ModelWindow: 1000})
	messages := conversation(3)
	// A cut of zero or less means nothing older than the tail could be folded.
	// Everything here fits the budget, so everything is kept: walking the cut
	// from the end instead would have returned an empty history.
	kept, tokens := manager.forceTruncate(messages, CompactPlan{Cut: -5})
	if len(kept) != len(messages) {
		t.Fatalf("kept = %d of %d", len(kept), len(messages))
	}
	if tokens != manager.Estimate(messages) {
		t.Fatalf("tokens = %d", tokens)
	}
	// A cut past the end keeps nothing, which is the caller's stated intent.
	kept, _ = manager.forceTruncate(messages, CompactPlan{Cut: 999})
	if len(kept) != 0 {
		t.Fatalf("kept = %d", len(kept))
	}
	// A tail that does not fit is trimmed to what does.
	tight := newTestManager(t, Config{ModelWindow: 400})
	kept, _ = tight.forceTruncate(messages, CompactPlan{Cut: 0})
	if len(kept) == 0 {
		t.Fatal("an oversized tail was trimmed to nothing")
	}
	if len(kept) >= len(messages) {
		t.Fatalf("kept = %d of %d, nothing was trimmed", len(kept), len(messages))
	}
	if tight.Estimate(kept) > tight.BudgetTokens() {
		t.Fatalf("the trimmed tail is still over budget: %d", tight.Estimate(kept))
	}
	// The result is a copy, so a caller cannot append into the caller's history.
	kept, _ = manager.forceTruncate(messages, CompactPlan{Cut: 1})
	kept[0].Content[0].Text = "mutated"
	if messages[1].Content[0].Text == "mutated" {
		t.Fatal("the truncated result aliases the caller's messages")
	}
}

func TestNewSpillerReportsAnUnusableDirectory(t *testing.T) {
	// A file where the spill directory belongs is a configuration mistake that
	// has to surface now rather than on the first oversized result.
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewSpiller(SpillConfig{Directory: file}); err == nil {
		t.Fatal("a file was accepted as the spill directory")
	}
}

func TestHandleReportsAnUnwritableTarget(t *testing.T) {
	directory := t.TempDir()
	spiller := newTestSpiller(t, SpillConfig{
		Directory: directory, MaxInlineTokens: 10, SummaryTokens: 5,
	})
	payload := []byte(strings.Repeat("x", 500))
	// A directory sitting exactly where the spilled file belongs makes the write
	// fail, and the tool result is better off reported than silently dropped.
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	blocker := filepath.Join(directory, "s1", spillFileName("read", digest))
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := spiller.Handle(context.Background(), "s1", "c1", "read", payload)
	if err == nil {
		t.Fatal("an unwritable spill target was accepted")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestSummarizerFailurePropagates(t *testing.T) {
	sentinel := errors.New("no credit")
	strategy := &SummarizeStrategy{
		Summarizer: SummarizerFunc(func(context.Context, string) (string, error) {
			return "", sentinel
		}),
	}
	_, err := strategy.SummarizeRegion(
		context.Background(), []core.Message{user("x")}, CompactPlan{Cut: 1})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
}
