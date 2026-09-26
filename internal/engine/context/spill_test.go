// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func newTestSpiller(t *testing.T, config SpillConfig) *Spiller {
	t.Helper()
	if config.Directory == "" {
		config.Directory = t.TempDir()
	}
	spiller, err := NewSpiller(config)
	if err != nil {
		t.Fatal(err)
	}
	return spiller
}

func TestNewSpillerValidatesItsConfiguration(t *testing.T) {
	if _, err := NewSpiller(SpillConfig{}); err == nil {
		t.Fatal("a spiller without a directory was accepted")
	}
	// A summary that cannot fit its own budget would silently produce a
	// reference larger than the result it replaced.
	if _, err := NewSpiller(SpillConfig{
		Directory:       t.TempDir(),
		MaxInlineTokens: 10,
		SummaryTokens:   100,
	}); err == nil {
		t.Fatal("an impossible summary budget was accepted")
	}
	spiller := newTestSpiller(t, SpillConfig{})
	if spiller.Directory() == "" {
		t.Fatal("the spill directory was not resolved")
	}
}

func TestSmallResultsStayInline(t *testing.T) {
	spiller := newTestSpiller(t, SpillConfig{MaxInlineTokens: 1000})
	result, err := spiller.Handle(context.Background(), "s1", "c1", "read", []byte("a small result"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Spilled {
		t.Fatal("a small result was spilled")
	}
	if result.Tokens == 0 || result.Bytes == 0 {
		t.Fatalf("result = %#v", result)
	}
	// Nothing was written, so there is nothing to clean up.
	entries, err := os.ReadDir(spiller.Directory())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the spill directory holds %d entries", len(entries))
	}
}

func TestLargeResultsAreSpilledAndReferenced(t *testing.T) {
	spiller := newTestSpiller(t, SpillConfig{MaxInlineTokens: 300, SummaryTokens: 20})
	payload := []byte(strings.Repeat("the whole file content ", 200))
	result, err := spiller.Handle(context.Background(), "s1", "c1", "read_file", payload)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Spilled {
		t.Fatal("a large result was not spilled")
	}
	if result.Bytes != len(payload) {
		t.Fatalf("bytes = %d, want %d", result.Bytes, len(payload))
	}
	if result.Tokens >= 300 {
		t.Fatalf("the reference costs %d tokens, more than the budget", result.Tokens)
	}
	if result.OverBudget {
		t.Fatal("the reference was reported as over budget when it fits")
	}
	// The reference has to say where the data is, how to check it, and that it
	// is untrusted, or a model would either not know where to look or trust it.
	for _, wanted := range []string{
		SpillMarker, "read_file", "c1", filepath.ToSlash(result.Path), result.Digest[:16],
		"untrusted", "excerpt", "</tool_output_summary>",
	} {
		if !strings.Contains(result.Reference, wanted) {
			t.Fatalf("reference is missing %q:\n%s", wanted, result.Reference)
		}
	}
	if !strings.Contains(result.Reference, "the whole file content") {
		t.Fatalf("the excerpt is missing:\n%s", result.Reference)
	}
	// The full content is on disk and reads back exactly.
	read, err := ReadSpilled(result.Path, result.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if string(read) != string(payload) {
		t.Fatal("the spilled content did not round trip")
	}
	// The file is named after the tool and the content, not after the clock
	// alone, so a repeated result does not fill the directory with copies.
	again, err := spiller.Handle(context.Background(), "s1", "c1", "read_file", payload)
	if err != nil {
		t.Fatal(err)
	}
	if again.Path != result.Path {
		t.Fatalf("the same content spilled to two paths: %q and %q", result.Path, again.Path)
	}
	count, bytes := spiller.Stats()
	if count != 2 || bytes != int64(2*len(payload)) {
		t.Fatalf("stats = %d records, %d bytes", count, bytes)
	}
}

// TestReferenceReportsAnImpossibleBudget covers the configuration the design
// cannot satisfy: an inline budget so small that even a path and a digest do not
// fit. The reference is still produced, because it is the minimum a model needs,
// but the caller is told that spilling did not buy what it promised.
func TestReferenceReportsAnImpossibleBudget(t *testing.T) {
	spiller := newTestSpiller(t, SpillConfig{MaxInlineTokens: 5, SummaryTokens: 2})
	payload := []byte(strings.Repeat("content ", 500))
	result, err := spiller.Handle(context.Background(), "s1", "c1", "read", payload)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Spilled {
		t.Fatal("the result was not spilled")
	}
	if !result.OverBudget {
		t.Fatalf("a %d token reference was not reported against a budget of 5", result.Tokens)
	}
	// The excerpt is what gives way first, so the data can still be read back.
	if strings.Contains(result.Reference, "excerpt=") {
		t.Fatalf("the excerpt was kept in an impossible budget:\n%s", result.Reference)
	}
	for _, wanted := range []string{filepath.ToSlash(result.Path), result.Digest[:16], "untrusted"} {
		if !strings.Contains(result.Reference, wanted) {
			t.Fatalf("reference is missing %q:\n%s", wanted, result.Reference)
		}
	}
	// It is still a real spill, so the data is recoverable.
	read, err := ReadSpilled(result.Path, result.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if string(read) != string(payload) {
		t.Fatal("the spilled content is not the original")
	}
}

func TestReadSpilledDetectsChangedContent(t *testing.T) {
	spiller := newTestSpiller(t, SpillConfig{MaxInlineTokens: 20, SummaryTokens: 10})
	payload := []byte(strings.Repeat("content ", 100))
	result, err := spiller.Handle(context.Background(), "s1", "c1", "read", payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result.Path, []byte("something else"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Acting on content nobody audited is exactly what the digest prevents.
	if _, err := ReadSpilled(result.Path, result.Digest); err == nil {
		t.Fatal("changed content was accepted")
	}
	// Reading without a digest is allowed, for a caller that has its own check.
	if _, err := ReadSpilled(result.Path, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSpilled(filepath.Join(t.TempDir(), "absent"), ""); err == nil {
		t.Fatal("a missing spilled file was read")
	}
}

func TestSpillRejectsAPathEscapingSessionID(t *testing.T) {
	spiller := newTestSpiller(t, SpillConfig{MaxInlineTokens: 10, SummaryTokens: 5})
	payload := []byte(strings.Repeat("x", 5000))
	for _, sessionID := range []string{"", "   ", "..", "../escape", `a\b`, "a:b", "a\x00b"} {
		if _, err := spiller.Handle(context.Background(), sessionID, "c1", "read", payload); err == nil {
			t.Fatalf("session id %q was accepted", sessionID)
		}
	}
}

func TestApplySpilledRewritesOnlyTheOversizedResults(t *testing.T) {
	ctx := context.Background()
	spiller := newTestSpiller(t, SpillConfig{MaxInlineTokens: 40, SummaryTokens: 10})
	small := []byte("small output")
	large := []byte(strings.Repeat("large output ", 200))
	messages := []core.Message{
		user("do the thing"),
		{Role: core.RoleTool, Content: []core.ContentBlock{{
			Type: core.ContentTypeToolResult, ToolName: "read", ToolCallID: "c1", Output: small,
		}}},
		{Role: core.RoleTool, Content: []core.ContentBlock{{
			Type: core.ContentTypeToolResult, ToolName: "grep", ToolCallID: "c2", Output: large,
			IsError: true,
		}}},
		assistant("done"),
	}
	rewritten, results, err := spiller.ApplySpilled(ctx, "s1", messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	if results[0].Spilled || !results[1].Spilled {
		t.Fatalf("results = %#v", results)
	}
	// The small result is untouched, byte for byte.
	if string(rewritten[1].Content[0].Output) != string(small) {
		t.Fatal("a small result was altered")
	}
	// The large one becomes a reference, and the error flag survives, because
	// a failed call stays a failed call after being spilled.
	spilled := rewritten[2].Content[0]
	if !spilled.IsError {
		t.Fatal("the error flag was lost")
	}
	if spilled.ToolName != "grep" || spilled.ToolCallID != "c2" {
		t.Fatalf("spilled block = %#v", spilled)
	}
	var reference struct {
		SpilledTo string `json:"spilled_to"`
		Digest    string `json:"digest"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal(spilled.Output, &reference); err != nil {
		t.Fatal(err)
	}
	if reference.SpilledTo == "" || reference.Digest == "" || reference.Summary == "" {
		t.Fatalf("reference = %#v", reference)
	}
	read, err := ReadSpilled(filepath.FromSlash(reference.SpilledTo), reference.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if string(read) != string(large) {
		t.Fatal("the spilled content is not the original")
	}
	// The caller's messages are untouched, so a failed spill can be reported
	// without the history already being rewritten.
	if string(messages[2].Content[0].Output) != string(large) {
		t.Fatal("the caller's messages were rewritten")
	}
	// A message with no tool results is carried over unchanged.
	if len(rewritten[0].Content) != 1 || len(rewritten[3].Content) != 1 {
		t.Fatal("a message without results was altered")
	}
}

func TestApplySpilledLeavesAnEmptyConversationAlone(t *testing.T) {
	spiller := newTestSpiller(t, SpillConfig{})
	rewritten, results, err := spiller.ApplySpilled(context.Background(), "s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewritten) != 0 || len(results) != 0 {
		t.Fatalf("rewritten = %#v results = %#v", rewritten, results)
	}
	// A tool result with no output is left alone rather than spilled as empty.
	messages := []core.Message{{
		Role: core.RoleTool, Content: []core.ContentBlock{{Type: core.ContentTypeToolResult, ToolName: "x"}},
	}}
	rewritten, _, err = spiller.ApplySpilled(context.Background(), "s1", messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(rewritten[0].Content[0].Output) != 0 {
		t.Fatal("an empty result gained content")
	}
}

func TestSpillFileNamesAreSafeAndStable(t *testing.T) {
	name := spillFileName("../../etc/passwd", "0123456789abcdef")
	if strings.ContainsAny(name, `/\`) {
		t.Fatalf("name = %q", name)
	}
	if !strings.HasSuffix(name, "0123456789abcdef.txt") {
		t.Fatalf("name = %q", name)
	}
	if spillFileName("", "0123456789abcdef") == "" {
		t.Fatal("a nameless tool produced no file name")
	}
	// A long or unusual tool name is still a safe component.
	for _, toolName := range []string{strings.Repeat("x", 100), "with space", "üñïçø∂é"} {
		safe := sanitizeFileComponent(toolName)
		if strings.ContainsAny(safe, `/\:`) {
			t.Fatalf("tool %q produced %q", toolName, safe)
		}
		if len(safe) > 32 {
			t.Fatalf("tool %q produced %q, longer than the cap", toolName, safe)
		}
	}
	if sanitizeFileComponent("///") == "" {
		t.Log("a name of only separators sanitizes to nothing, which the caller defaults")
	}
}

func TestValidateID(t *testing.T) {
	for _, id := range []string{"", "  ", ".", "..", "a/b", `a\b`, "a:b", "a\x00b"} {
		if err := ValidateID("session id", id); err == nil {
			t.Fatalf("id %q was accepted", id)
		}
	}
	if err := ValidateID("session id", "s1-2_3"); err != nil {
		t.Fatal(err)
	}
}

func TestRingBufferDropsTheOldest(t *testing.T) {
	var mu sync.Mutex
	var dropped []int
	buffer, err := NewRingBuffer(3, OverflowHandlerFunc(func(count int, total int) {
		mu.Lock()
		defer mu.Unlock()
		dropped = append(dropped, count)
		if total < count {
			t.Errorf("total %d is below the drop count %d", total, count)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if dropped := buffer.Append(user("one"), user("two")); dropped != 0 {
		t.Fatalf("dropped = %d with room to spare", dropped)
	}
	if buffer.Len() != 2 {
		t.Fatalf("len = %d", buffer.Len())
	}
	// One more fits exactly.
	buffer.Append(user("three"))
	if buffer.Append(user("four")) != 1 {
		t.Fatal("overflow was not reported")
	}
	buffer.Append(user("five"), user("six"))
	messages := buffer.Messages()
	if len(messages) != 3 {
		t.Fatalf("len = %d", len(messages))
	}
	// The survivors are the newest, in order.
	for index, wanted := range []string{"four", "five", "six"} {
		if messages[index].Content[0].Text != wanted {
			t.Fatalf("message %d = %q, want %q", index, messages[index].Content[0].Text, wanted)
		}
	}
	if buffer.Dropped() != 3 {
		t.Fatalf("dropped total = %d", buffer.Dropped())
	}
	if buffer.Capacity() != 3 {
		t.Fatalf("capacity = %d", buffer.Capacity())
	}
	// The handler runs off the caller's goroutine, so wait for it rather than
	// assuming it already ran.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		count := len(dropped)
		mu.Unlock()
		if count >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := len(dropped)
	mu.Unlock()
	if got < 2 {
		t.Fatalf("the overflow handler ran %d times, want at least 2", got)
	}
}

func TestRingBufferHandsOutCopies(t *testing.T) {
	buffer, err := NewRingBuffer(2, nil)
	if err != nil {
		t.Fatal(err)
	}
	buffer.Append(user("original"))
	messages := buffer.Messages()
	messages[0].Content[0].Text = "mutated"
	if buffer.Messages()[0].Content[0].Text != "original" {
		t.Fatal("the caller mutated the buffer's messages")
	}
	// Reset keeps the drop count, because a reset is not a reason to forget that
	// history was lost.
	buffer.Append(user("a"), user("b"), user("c"))
	buffer.Reset()
	if buffer.Len() != 0 {
		t.Fatalf("len = %d", buffer.Len())
	}
	if buffer.Dropped() != 2 {
		t.Fatalf("dropped = %d", buffer.Dropped())
	}
}

func TestNewRingBufferRejectsAnImpossibleCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		if _, err := NewRingBuffer(capacity, nil); err == nil {
			t.Fatalf("capacity %d was accepted", capacity)
		}
	}
}
