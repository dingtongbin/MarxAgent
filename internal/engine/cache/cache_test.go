// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func tool(name, description string) core.ToolSpec {
	return core.ToolSpec{
		Name:        name,
		Description: description,
		Parameters:  []byte(`{"type":"object"}`),
	}
}

func history(count int) []core.Message {
	messages := make([]core.Message, 0, count)
	for index := 0; index < count; index++ {
		messages = append(messages, core.Message{
			Role:    core.RoleUser,
			Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "turn"}},
		})
	}
	return messages
}

func TestSignatureIsStableAcrossCalls(t *testing.T) {
	manager := New(Config{})
	signature := manager.Signature("you are helpful", []core.ToolSpec{tool("read", "reads"), tool("bash", "runs")}, "index")
	for attempt := 0; attempt < 10; attempt++ {
		if again := manager.Signature("you are helpful", []core.ToolSpec{tool("bash", "runs"), tool("read", "reads")}, "index"); again != signature {
			t.Fatalf("the signature changed between calls: %s then %s", signature, again)
		}
	}
	if signature == "" {
		t.Fatal("an empty signature was produced")
	}
}

func TestSignatureIgnoresToolOrderButNotToolContent(t *testing.T) {
	manager := New(Config{})
	first := manager.Signature("system", []core.ToolSpec{tool("a", "one"), tool("b", "two")}, "index")
	// A registry returning the same tools in a different order must not look like
	// a changed prompt, or every turn would pay full price.
	reordered := manager.Signature("system", []core.ToolSpec{tool("b", "two"), tool("a", "one")}, "index")
	if reordered != first {
		t.Fatal("reordering the tools changed the signature")
	}
	// A changed description is a real change: the provider caches bytes.
	if manager.Signature("system", []core.ToolSpec{tool("a", "one"), tool("b", "changed")}, "index") == first {
		t.Fatal("a changed description was not detected")
	}
	if manager.Signature("system", []core.ToolSpec{tool("a", "one")}, "index") == first {
		t.Fatal("a removed tool was not detected")
	}
	if manager.Signature("changed", []core.ToolSpec{tool("a", "one"), tool("b", "two")}, "index") == first {
		t.Fatal("a changed system prompt was not detected")
	}
}

func TestSignatureSeparatesFieldsSoTheyCannotCollide(t *testing.T) {
	manager := New(Config{})
	// Without length prefixes, a prompt ending in "ab" and one ending in "a" with
	// a tool named "b" could hash the same. The digest is the only thing standing
	// between a cache hit and a stale answer, so it is worth a test.
	first := manager.Signature("ab", []core.ToolSpec{tool("c", "d")}, "index")
	second := manager.Signature("a", []core.ToolSpec{tool("bc", "d")}, "index")
	if first == second {
		t.Fatal("two different prefixes produced the same signature")
	}
}

func TestFirstRecordHasNothingToCompareAgainst(t *testing.T) {
	manager := New(Config{})
	snapshot, change := manager.Record("system", []core.ToolSpec{tool("a", "one")}, "", "", history(2))
	if change.Kind != ChangeNone {
		t.Fatalf("change = %#v", change)
	}
	if snapshot.Signature == "" {
		t.Fatal("the first snapshot has no signature")
	}
	if snapshot.MessageCount != 2 {
		t.Fatalf("message count = %d", snapshot.MessageCount)
	}
	// The sizes make it obvious which layer is churning when something goes wrong.
	for _, layer := range []Layer{LayerSystem, LayerTools, LayerIndex, LayerHistory, LayerDynamic} {
		if _, ok := snapshot.LayerSizes[layer]; !ok {
			t.Fatalf("layer %q has no size", layer)
		}
	}
	if snapshot.LayerSizes[LayerSystem] != len("system") {
		t.Fatalf("system size = %d", snapshot.LayerSizes[LayerSystem])
	}
	if snapshot.LayerSizes[LayerTools] == 0 {
		t.Fatal("the tools contributed nothing")
	}
}

func TestAppendingHistoryKeepsTheCacheValid(t *testing.T) {
	manager := New(Config{})
	_, _ = manager.Record("system", []core.ToolSpec{tool("a", "one")}, "index", "dynamic one", history(2))
	signature := manager.Current().Signature
	for turn := 3; turn <= 20; turn++ {
		_, change := manager.Record(
			"system", []core.ToolSpec{tool("a", "one")},
			"index", "dynamic "+strings.Repeat("x", turn), history(turn))
		if change.Kind != ChangeAppend {
			t.Fatalf("turn %d: change = %#v", turn, change)
		}
		if change.From != signature || change.To != signature {
			t.Fatalf("an append changed the signature: %s to %s", change.From, change.To)
		}
	}
}

func TestEditingThePromptOrToolsInvalidates(t *testing.T) {
	manager := New(Config{})
	_, _ = manager.Record("system", []core.ToolSpec{tool("a", "one")}, "", "", history(4))
	_, change := manager.Record("system edited", []core.ToolSpec{tool("a", "one")}, "", "", history(4))
	if change.Kind != ChangeEdit {
		t.Fatalf("change = %#v", change)
	}
	if change.From == change.To {
		t.Fatal("an edit did not change the signature")
	}
	if !strings.Contains(change.Reason, "changed") {
		t.Fatalf("reason = %q", change.Reason)
	}
	// A changed capability index is also an edit, because it sits above the
	// history in the cached prefix.
	manager2 := New(Config{})
	_, _ = manager2.Record("system", []core.ToolSpec{tool("a", "one")}, "index one", "", history(4))
	_, change = manager2.Record("system", []core.ToolSpec{tool("a", "one")}, "index one and two", "", history(4))
	if change.Kind != ChangeEdit {
		t.Fatalf("an index change was not an edit: %#v", change)
	}
}

func TestShrinkingHistoryIsNotAnAppend(t *testing.T) {
	manager := New(Config{})
	_, _ = manager.Record("system", nil, "", "", history(20))
	// A shrinking history is a compaction, which rewrote the prefix rather than
	// growing it, so treating it as an append would report a cache hit that the
	// provider cannot give.
	_, change := manager.Record("system", nil, "", "", history(5))
	if change.Kind != ChangeEdit {
		t.Fatalf("change = %#v", change)
	}
}

func TestDynamicTailIsExcludedFromTheSignature(t *testing.T) {
	manager := New(Config{})
	_, _ = manager.Record("system", nil, "", "first injection", history(2))
	signature := manager.Current().Signature
	// Content injected for one turn changes every turn by design. If it reached
	// the signature, the cache would never hit.
	for turn := 0; turn < 10; turn++ {
		snapshot, change := manager.Record(
			"system", nil, "", strings.Repeat("injected ", turn+1), history(2+turn))
		// Every turn after the first grew its history, so every one of them is an
		// append, and none of them moved the signature.
		if turn > 0 && change.Kind != ChangeAppend {
			t.Fatalf("turn %d: change = %#v", turn, change)
		}
		if snapshot.Signature != signature {
			t.Fatal("the dynamic tail changed the signature")
		}
		if snapshot.DynamicBytes == 0 {
			t.Fatal("the dynamic tail was not measured")
		}
	}
	if !Stable(LayerHistory) || Stable(LayerDynamic) {
		t.Fatal("the layer stability table is wrong")
	}
	if Stable("invented") {
		t.Fatal("an unknown layer was treated as stable")
	}
}

func TestCacheKeyDistinguishesTurnsSharingAPrefix(t *testing.T) {
	manager := New(Config{})
	if key := manager.CacheKey(); key != "" {
		t.Fatalf("a key was produced before anything was recorded: %q", key)
	}
	_, _ = manager.Record("system", nil, "", "", history(1))
	first := manager.CacheKey()
	if first == "" {
		t.Fatal("no cache key was produced")
	}
	_, _ = manager.Record("system", nil, "", "something new", history(2))
	second := manager.CacheKey()
	// The prefix is byte identical across both turns, so a key derived from the
	// prefix alone would be the same, and a provider reusing it would return the
	// first turn's answer.
	if first == second {
		t.Fatal("two turns sharing a prefix produced the same cache key")
	}
	// The volatile tail is deliberately not part of the key: caching it would be
	// pointless, and changing it must not look like the prefix changed.
	_, _ = manager.Record("system", nil, "", "something else", history(2))
	if manager.CacheKey() != second {
		t.Fatal("the dynamic tail changed the cache key")
	}
	// Growing the history does change the key, because the provider has to be
	// asked about a prefix it has not seen.
	_, _ = manager.Record("system", nil, "", "something else", history(3))
	if manager.CacheKey() == second {
		t.Fatal("growing the history did not change the cache key")
	}
}

func TestInvalidateDiscardsThePrefix(t *testing.T) {
	manager := New(Config{})
	_, _ = manager.Record("system", nil, "", "", history(2))
	change := manager.Invalidate("the caller changed something this package cannot see")
	if change.Kind != ChangeEdit {
		t.Fatalf("change = %#v", change)
	}
	if change.From == "" {
		t.Fatal("the previous signature was not reported")
	}
	if manager.CacheKey() != "" {
		t.Fatal("a key survived the invalidation")
	}
	invalidated, count := manager.Invalidated()
	if !invalidated || count != 1 {
		t.Fatalf("invalidated = %v, count = %d", invalidated, count)
	}
	// A snapshot after the invalidation is a fresh start.
	_, change = manager.Record("system", nil, "", "", history(2))
	if change.Kind != ChangeNone {
		t.Fatalf("change = %#v", change)
	}
}

func TestResetIsDistinctFromAnEdit(t *testing.T) {
	manager := New(Config{})
	_, _ = manager.Record("system", nil, "", "", history(10))
	change := manager.Reset("compaction folded the history")
	// A reset is not an edit: the caller knows it happened on purpose, and a
	// diagnostic that called it an edit would send someone hunting for a caller
	// that changed the prompt.
	if change.Kind != ChangeReset {
		t.Fatalf("change = %#v", change)
	}
	if change.To != change.From {
		t.Fatal("a reset changed the signature")
	}
	stats := manager.Stats()
	if stats.Resets != 1 || stats.Edits != 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestObserveAlarmsOnALowHitRate(t *testing.T) {
	manager := New(Config{ObservationWindow: 4})
	// A healthy run says nothing.
	for turn := 0; turn < 4; turn++ {
		_, alarmed := manager.Observe(Observation{
			ReadTokens: 900, WriteTokens: 100, InputTokens: 1000,
		})
		if alarmed {
			t.Fatal("a healthy run raised an alarm")
		}
	}
	// A collapse is reported, along with what the prefix was doing around it.
	_, _ = manager.Record("system", []core.ToolSpec{tool("a", "one")}, "", "", history(4))
	_, change := manager.Record("system", []core.ToolSpec{tool("a", "one")}, "", "", history(5))
	if change.Kind != ChangeAppend {
		t.Fatalf("change = %#v", change)
	}
	var alarm Alarm
	var raised bool
	for turn := 0; turn < 4; turn++ {
		// The alarm is edge triggered, so it fires on the turn the rate crosses the
		// threshold and not on the turns that follow. The flag is accumulated for
		// that reason.
		current, fired := manager.Observe(Observation{ReadTokens: 10, InputTokens: 1000})
		if fired {
			alarm, raised = current, true
		}
	}
	if !raised {
		t.Fatal("a collapsed hit rate raised no alarm")
	}
	if alarm.HitRate >= DefaultLowHitRateThreshold {
		t.Fatalf("hit rate = %f", alarm.HitRate)
	}
	if alarm.Threshold != DefaultLowHitRateThreshold {
		t.Fatalf("threshold = %f", alarm.Threshold)
	}
	if alarm.Window != 4 {
		t.Fatalf("window = %d", alarm.Window)
	}
	if !strings.Contains(alarm.Message, "hit rate") {
		t.Fatalf("message = %q", alarm.Message)
	}
	if alarm.Change.Kind != ChangeAppend {
		t.Fatalf("the alarm did not carry the surrounding change: %#v", alarm.Change)
	}
	if len(manager.Alarms()) != 1 {
		t.Fatalf("alarms = %d, want one", len(manager.Alarms()))
	}
	// Sustaining the bad rate does not repeat the alarm. A line that appears every
	// turn of an outage is a line nobody reads.
	_, repeated := manager.Observe(Observation{ReadTokens: 5, InputTokens: 1000})
	if repeated {
		t.Fatal("the outage was reported again")
	}
	if len(manager.Alarms()) != 1 {
		t.Fatalf("alarms = %d, want one", len(manager.Alarms()))
	}
	// Recovery rearms it, so a later collapse is a new event. The window has to be
	// refilled with healthy turns and then dragged back down, because a rate
	// averaged over a window does not collapse the instant one turn is bad.
	for turn := 0; turn < 4; turn++ {
		manager.Observe(Observation{ReadTokens: 990, InputTokens: 1000})
	}
	raised = false
	for turn := 0; turn < 4 && !raised; turn++ {
		_, raised = manager.Observe(Observation{ReadTokens: 5, InputTokens: 1000})
	}
	if len(manager.Alarms()) != 2 {
		t.Fatalf("alarms = %d", len(manager.Alarms()))
	}
}

func TestObserveStaysQuietUntilTheWindowIsFull(t *testing.T) {
	// Alarming on two bad turns would train a reader to ignore the alarm, so the
	// window has to fill first.
	manager := New(Config{ObservationWindow: 5})
	for turn := 0; turn < 4; turn++ {
		if _, alarmed := manager.Observe(Observation{InputTokens: 1000}); alarmed {
			t.Fatalf("an alarm was raised on turn %d of 5", turn)
		}
	}
	if _, alarmed := manager.Observe(Observation{InputTokens: 1000}); !alarmed {
		t.Fatal("no alarm once the window was full")
	}
}

func TestObserveWithNoInputTokensDoesNotDivideByZero(t *testing.T) {
	manager := New(Config{ObservationWindow: 2})
	for turn := 0; turn < 4; turn++ {
		if _, alarmed := manager.Observe(Observation{}); alarmed {
			t.Fatal("an empty observation raised an alarm")
		}
	}
	rate, window := manager.HitRate()
	if rate != 0 || window != 2 {
		t.Fatalf("rate = %f over %d turns", rate, window)
	}
}

func TestObservationHitRate(t *testing.T) {
	if (Observation{}).HitRate() != 0 {
		t.Fatal("an empty observation reported a rate")
	}
	observation := Observation{ReadTokens: 250, InputTokens: 1000}
	if observation.HitRate() != 0.25 {
		t.Fatalf("rate = %f", observation.HitRate())
	}
}

func TestAlarmsAreBounded(t *testing.T) {
	manager := New(Config{ObservationWindow: 1, MaxHistory: 4})
	for turn := 0; turn < 100; turn++ {
		manager.Observe(Observation{InputTokens: 1000, At: time.Unix(int64(turn), 0)})
	}
	if len(manager.Alarms()) > manager.maxAlarm {
		t.Fatalf("alarms = %d", len(manager.Alarms()))
	}
	if len(manager.Changes()) > 4 {
		t.Fatalf("changes = %d", len(manager.Changes()))
	}
}

func TestStatsSummarizesBothHalves(t *testing.T) {
	manager := New(Config{ObservationWindow: 2})
	_, _ = manager.Record("system", nil, "", "", history(2))
	_, _ = manager.Record("system", nil, "", "", history(4))
	_, _ = manager.Record("changed", nil, "", "", history(4))
	manager.Reset("compaction")
	for turn := 0; turn < 4; turn++ {
		manager.Observe(Observation{ReadTokens: 500, WriteTokens: 100, InputTokens: 1000})
	}
	stats := manager.Stats()
	if stats.Turns != 4 {
		t.Fatalf("turns = %d", stats.Turns)
	}
	if stats.ReadTokens != 2000 || stats.InputTokens != 4000 {
		t.Fatalf("tokens = %#v", stats)
	}
	if stats.HitRate != 0.5 || stats.Window != 2 {
		t.Fatalf("rate = %f over %d", stats.HitRate, stats.Window)
	}
	if stats.Appends != 1 || stats.Edits != 1 || stats.Resets != 1 {
		t.Fatalf("changes = %#v", stats)
	}
}

func TestBuildReturnsAnOrderedPrefixExcludingTheTail(t *testing.T) {
	manager := New(Config{})
	prefix := manager.Build(
		"the system prompt",
		[]core.ToolSpec{tool("zebra", "last"), tool("apple", "first")},
		"the capability index",
		history(3),
		"the volatile injection",
	)
	if prefix.Tools[0].Name != "apple" {
		t.Fatalf("tools are not ordered: %#v", prefix.Tools)
	}
	if prefix.Signature == "" || prefix.Key == "" {
		t.Fatalf("prefix = %#v", prefix)
	}
	// The volatile content is returned so a caller can be shown it was excluded
	// rather than having to trust that it was.
	if prefix.Dynamic != "the volatile injection" {
		t.Fatalf("dynamic = %q", prefix.Dynamic)
	}
	if !Stable(LayerIndex) {
		t.Fatal("the capability index should be a stable layer")
	}
	// The history is copied, so a caller rewriting it cannot reach the manager's
	// own record.
	prefix.History[0].Content[0].Text = "mutated"
	again := manager.Build("the system prompt", []core.ToolSpec{tool("zebra", "last"), tool("apple", "first")},
		"the capability index", history(3), "the volatile injection")
	if again.History[0].Content[0].Text == "mutated" {
		t.Fatal("the history aliases the manager's state")
	}
}

func TestSnapshotDescribeAndMarshal(t *testing.T) {
	manager := New(Config{})
	snapshot, _ := manager.Record("system", []core.ToolSpec{tool("a", "one")}, "index", "dynamic", history(2))
	described := snapshot.Describe()
	for _, wanted := range []string{"turn 1", "messages 2", "system=6", "tools="} {
		if !strings.Contains(described, wanted) {
			t.Fatalf("description is missing %q: %s", wanted, described)
		}
	}
	encoded, err := snapshot.MarshalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, `"message_count":2`) {
		t.Fatalf("encoded = %s", encoded)
	}
	// A snapshot with no signature still describes itself, because a log line
	// reading "none" is useful and a panic is not.
	empty := Snapshot{}
	if !strings.Contains(empty.Describe(), "none") {
		t.Fatalf("description = %s", empty.Describe())
	}
}

func TestCurrentAndChangesHandOutCopies(t *testing.T) {
	manager := New(Config{})
	_, _ = manager.Record("system", nil, "", "", history(2))
	changes := manager.Changes()
	changes[0].Kind = "tampered"
	if manager.Changes()[0].Kind == "tampered" {
		t.Fatal("the caller mutated the manager's changes")
	}
	// The snapshot the caller kept describes the turn it was taken on, and does
	// not follow later records.
	first := manager.Current()
	firstCount := first.MessageCount
	_, _ = manager.Record("system changed", nil, "", "", history(9))
	if manager.Current().MessageCount != 9 {
		t.Fatal("the current snapshot was not updated")
	}
	if first.MessageCount != firstCount {
		t.Fatal("an earlier snapshot changed under the caller")
	}
}
