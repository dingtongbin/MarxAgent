// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func user(text string) core.Message {
	return core.Message{
		Role:    core.RoleUser,
		Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: text}},
	}
}

func assistant(text string) core.Message {
	return core.Message{
		Role:    core.RoleAssistant,
		Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: text}},
	}
}

func toolResult(name, output string, isError bool) core.Message {
	return core.Message{
		Role: core.RoleTool,
		Content: []core.ContentBlock{{
			Type:     core.ContentTypeToolResult,
			ToolName: name,
			Output:   []byte(output),
			IsError:  isError,
		}},
	}
}

func conversation(turns int) []core.Message {
	var messages []core.Message
	for turn := 0; turn < turns; turn++ {
		messages = append(messages,
			user("please look at file "+strings.Repeat("x", 200)),
			assistant("here is what I found "+strings.Repeat("y", 200)),
		)
	}
	return messages
}

func newTestManager(t *testing.T, config Config) *Manager {
	t.Helper()
	if config.ModelWindow == 0 {
		config.ModelWindow = 4000
	}
	manager, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestNewRejectsAnImpossibleWindow(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a manager without a model window was accepted")
	}
	if _, err := New(Config{ModelWindow: 100, MaxMessageTokens: -1}); err == nil {
		t.Fatal("a negative message cap was accepted")
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	manager := newTestManager(t, Config{})
	config := manager.Config()
	if config.MaxContextRatio != DefaultMaxContextRatio {
		t.Fatalf("ratio = %v", config.MaxContextRatio)
	}
	if config.KeepRecentTurns != DefaultKeepRecentTurns {
		t.Fatalf("keep recent turns = %d", config.KeepRecentTurns)
	}
	if config.Strategy == nil || config.Estimator == nil {
		t.Fatal("a default strategy or estimator was left nil")
	}
	// A ratio above one is a configuration mistake, not a request for a bigger
	// window, so it falls back rather than being honoured.
	odd := newTestManager(t, Config{MaxContextRatio: 4})
	if odd.Config().MaxContextRatio != DefaultMaxContextRatio {
		t.Fatalf("ratio = %v", odd.Config().MaxContextRatio)
	}
	if manager.BudgetTokens() != 2800 {
		t.Fatalf("budget = %d", manager.BudgetTokens())
	}
}

func TestCompactLeavesAConversationWithinBudgetAlone(t *testing.T) {
	manager := newTestManager(t, Config{KeepRecentTurns: 10})
	messages := conversation(3)
	compacted, report, err := manager.Compact(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(compacted) != len(messages) {
		t.Fatalf("a conversation within budget was compacted: %d became %d",
			len(messages), len(compacted))
	}
	if report.Strategy != "none" || report.Dropped != 0 {
		t.Fatalf("report = %#v", report)
	}
	if manager.ShouldCompact(messages) {
		t.Fatal("a conversation within budget was reported as over budget")
	}
	// A report is still recorded, so a caller can tell the difference between
	// "not compacted" and "not looked at".
	if manager.LastReport().TokensBefore == 0 {
		t.Fatal("no report was recorded")
	}
}

func TestCompactFoldsTheOlderRegionAway(t *testing.T) {
	var summarized strings.Builder
	manager := newTestManager(t, Config{
		ModelWindow:     2000,
		KeepRecentTurns: 2,
		Strategy: &SummarizeStrategy{
			Summarizer: SummarizerFunc(func(_ context.Context, text string) (string, error) {
				summarized.WriteString(text)
				return "the user asked about files repeatedly", nil
			}),
		},
		CacheInvalidator: CacheInvalidatorFunc(func(reason string) {
			if !strings.Contains(reason, "compaction") {
				t.Errorf("cache reason = %q", reason)
			}
		}),
		ResponseChainInvalidator: ResponseChainInvalidatorFunc(func(string) {}),
	})
	messages := conversation(20)
	if !manager.ShouldCompact(messages) {
		t.Fatal("a long conversation was not reported as over budget")
	}
	compacted, report, err := manager.Compact(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if report.Strategy != "summarize" || report.FellBack {
		t.Fatalf("report = %#v", report)
	}
	if report.Dropped == 0 {
		t.Fatal("nothing was folded away")
	}
	if report.TokensAfter >= report.TokensBefore {
		t.Fatalf("compaction did not shrink the conversation: %d became %d",
			report.TokensBefore, report.TokensAfter)
	}
	if len(compacted) >= len(messages) {
		t.Fatalf("messages = %d, started at %d", len(compacted), len(messages))
	}
	// The summary leads, and it is recognisable as a summary rather than as
	// something the user said.
	if len(compacted[0].Content) == 0 {
		t.Fatal("the result has no summary message")
	}
	summaryText := compacted[0].Content[0].Text
	if !strings.Contains(summaryText, DefaultSummaryHeader) {
		t.Fatalf("summary = %q", summaryText)
	}
	if !strings.Contains(summaryText, "the user asked about files repeatedly") {
		t.Fatalf("summary text missing: %q", summaryText)
	}
	if compacted[0].Role != SummaryRole {
		t.Fatalf("summary role = %q", compacted[0].Role)
	}
	if marked, ok := compacted[0].Metadata[SummaryMetadataKey].(bool); !ok || !marked {
		t.Fatalf("summary metadata = %#v", compacted[0].Metadata)
	}
	// The summarizer saw the folded region, not the tail.
	if !strings.Contains(summarized.String(), "please look at file") {
		t.Fatalf("the summarizer saw %q", summarized.String())
	}
	if !report.CacheInvalidated {
		t.Fatal("compaction did not reset the cached prefix")
	}
	if !report.ResponseChainInvalidated {
		t.Fatal("compaction did not invalidate the response chain")
	}
}

func TestCompactPreservesKeyToolResults(t *testing.T) {
	manager := newTestManager(t, Config{
		ModelWindow:     3000,
		KeepRecentTurns: 2,
		KeepKeyResults:  true,
		Strategy:        TruncateStrategy{},
	})
	messages := []core.Message{
		user("start"),
		toolResult("read", "the important file content", true),
		user("carry on"),
		assistant("sure"),
	}
	// Make the conversation long enough to force a fold.
	for turn := 0; turn < 20; turn++ {
		messages = append(messages,
			user(strings.Repeat("filler ", 200)),
			assistant(strings.Repeat("reply ", 200)),
		)
	}
	compacted, report, err := manager.Compact(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Preserved) != 1 {
		t.Fatalf("preserved = %v", report.Preserved)
	}
	// The failed tool result is somewhere in the result, verbatim, because it is
	// what the model has to reason about.
	found := false
	for _, message := range compacted {
		for _, block := range message.Content {
			if block.Type == core.ContentTypeToolResult && block.IsError {
				if !strings.Contains(string(block.Output), "the important file content") {
					t.Fatalf("the key result was altered: %s", block.Output)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the key tool result did not survive compaction")
	}
}

func TestCompactKeepsMetadataMarkedResults(t *testing.T) {
	marked := toolResult("grep", "a marked result", false)
	marked.Metadata = map[string]any{"key_result": true}
	if !isKeyResult(marked) {
		t.Fatal("a marked result was not treated as key")
	}
	plain := toolResult("grep", "an ordinary result", false)
	if isKeyResult(plain) {
		t.Fatal("an ordinary result was treated as key")
	}
}

func TestCompactFallsBackWhenTheStrategyFails(t *testing.T) {
	sentinel := errors.New("the summarizer is out of credit")
	manager := newTestManager(t, Config{
		ModelWindow:     2000,
		KeepRecentTurns: 2,
		Strategy: &SummarizeStrategy{
			Summarizer: SummarizerFunc(func(context.Context, string) (string, error) {
				return "", sentinel
			}),
		},
	})
	messages := conversation(20)
	compacted, report, err := manager.Compact(context.Background(), messages)
	// Losing detail is recoverable. Failing the turn is not.
	if err != nil {
		t.Fatalf("a strategy failure failed the turn: %v", err)
	}
	if !report.FellBack || report.Strategy != "truncate" {
		t.Fatalf("report = %#v", report)
	}
	if !errors.Is(report.Error, sentinel) {
		t.Fatalf("the reason was lost: %v", report.Error)
	}
	if !strings.Contains(report.FallbackReason, "out of credit") {
		t.Fatalf("reason = %q", report.FallbackReason)
	}
	if len(compacted) == 0 {
		t.Fatal("the fallback kept nothing")
	}
	if len(compacted) >= len(messages) {
		t.Fatalf("the fallback kept everything: %d", len(compacted))
	}
	if manager.LastReport().FellBack != true {
		t.Fatal("the fallback was not retained")
	}
}

func TestCompactFallsBackWithoutASummarizer(t *testing.T) {
	// A strategy that cannot work must fail loudly enough for the manager to
	// degrade on purpose, not by accident.
	manager := newTestManager(t, Config{
		ModelWindow:     2000,
		KeepRecentTurns: 2,
		Strategy:        &SummarizeStrategy{},
	})
	compacted, report, err := manager.Compact(context.Background(), conversation(20))
	if err != nil {
		t.Fatal(err)
	}
	if !report.FellBack {
		t.Fatalf("report = %#v", report)
	}
	if len(compacted) == 0 {
		t.Fatal("nothing was kept")
	}
}

func TestCompactFallsBackWhenNothingCanBeFolded(t *testing.T) {
	// A single turn larger than the whole window leaves nothing older than the
	// protected tail, so hard truncation is the only move and it has to be
	// reported rather than passed on as a successful compaction.
	manager := newTestManager(t, Config{
		ModelWindow:     300,
		KeepRecentTurns: 10,
		Strategy:        TruncateStrategy{},
	})
	messages := []core.Message{
		user(strings.Repeat("a very long question ", 200)),
		assistant(strings.Repeat("a very long answer ", 200)),
	}
	compacted, report, err := manager.Compact(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if !report.FellBack || report.Strategy != "truncate" {
		t.Fatalf("report = %#v", report)
	}
	if !strings.Contains(report.FallbackReason, "protected tail") {
		t.Fatalf("reason = %q", report.FallbackReason)
	}
	if len(compacted) == 0 {
		t.Fatal("hard truncation kept nothing, so the next turn has no context at all")
	}
	// One message alone does not fit either, and the report says so instead of
	// pretending the result is within budget.
	if !report.StillOverBudget {
		t.Fatalf("the result is over budget at %d of %d and the report did not say so",
			manager.Estimate(compacted), manager.BudgetTokens())
	}
}

func TestCompactIsANoOpWhenTheTailIsAllThereIs(t *testing.T) {
	manager := newTestManager(t, Config{KeepRecentTurns: 10})
	single := []core.Message{assistant("a short conversation")}
	compacted, report, err := manager.Compact(context.Background(), single)
	if err != nil {
		t.Fatal(err)
	}
	if report.Dropped != 0 {
		t.Fatalf("report = %#v", report)
	}
	if len(compacted) != 1 {
		t.Fatalf("compacted = %#v", compacted)
	}
}

func TestCompactDoesNotMutateTheCaller(t *testing.T) {
	manager := newTestManager(t, Config{
		ModelWindow: 2000, KeepRecentTurns: 2, Strategy: TruncateStrategy{},
	})
	messages := conversation(20)
	before := len(messages)
	first := messages[0]
	compacted, _, err := manager.Compact(context.Background(), messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != before {
		t.Fatal("compaction changed the caller's slice")
	}
	if messages[0].Role != first.Role || messages[0].Content[0].Text != first.Content[0].Text {
		t.Fatal("compaction changed the caller's messages")
	}
	// The result must not alias the input either, or a later append would
	// overwrite history that is still on the caller's side.
	compacted[0].Content[0].Text = "mutated"
	if messages[0].Content[0].Text == "mutated" {
		t.Fatal("the result aliases the caller's messages")
	}
}

func TestPlanProtectsRecentTurns(t *testing.T) {
	manager := newTestManager(t, Config{
		ModelWindow: 4000, KeepRecentTurns: 3,
	})
	messages := conversation(20)
	plan := manager.Plan(messages, manager.Estimate(messages))
	if plan.Cut <= 0 || plan.Cut >= len(messages) {
		t.Fatalf("cut = %d of %d", plan.Cut, len(messages))
	}
	if plan.KeepFrom < plan.Cut {
		t.Fatalf("keep from %d is before the cut %d", plan.KeepFrom, plan.Cut)
	}
	// A tool result that answers a tool call cannot be separated from it without
	// producing a conversation the provider rejects, so the cut may not land
	// between them.
	if messages[plan.Cut].Role == core.RoleTool {
		t.Fatalf("the cut landed on a tool message")
	}
}

func TestAdmitRefusesAnOversizedMessage(t *testing.T) {
	manager := newTestManager(t, Config{MaxMessageTokens: 300})
	if _, err := manager.Admit(conversation(2)); err != nil {
		t.Fatal(err)
	}
	huge := []core.Message{user(strings.Repeat("word ", 500))}
	if _, err := manager.Admit(huge); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err = %v", err)
	}
	// The messages that fit are still returned, so a caller can keep the turn.
	admitted, err := manager.Admit(append(conversation(1), huge...))
	if err == nil {
		t.Fatal("an oversized message was admitted")
	}
	if len(admitted) != 2 {
		t.Fatalf("admitted = %d", len(admitted))
	}
	// The message index is named, because "a message was too large" is not
	// something a caller can act on.
	if !strings.Contains(err.Error(), "message 2") {
		t.Fatalf("err = %v", err)
	}
	// A cap of zero means no cap.
	uncapped := newTestManager(t, Config{MaxMessageTokens: 0})
	if _, err := uncapped.Admit(huge); err != nil {
		t.Fatal(err)
	}
}

func TestReportsAreBoundedAndCopied(t *testing.T) {
	manager := newTestManager(t, Config{
		ModelWindow: 500, KeepRecentTurns: 1, Strategy: TruncateStrategy{},
		CacheInvalidator: CacheInvalidatorFunc(func(string) {}),
	})
	for turn := 0; turn < 80; turn++ {
		if _, _, err := manager.Compact(context.Background(), conversation(20)); err != nil {
			t.Fatal(err)
		}
	}
	reports := manager.Reports()
	if len(reports) > 64 {
		t.Fatalf("reports = %d, the history should be bounded", len(reports))
	}
	if len(reports) == 0 {
		t.Fatal("no reports were retained")
	}
	// The caller gets copies, so it cannot reach into the manager's state.
	if reports[0].Preserved != nil {
		reports[0].Preserved = append(reports[0].Preserved, 999)
		if len(manager.Reports()[0].Preserved) != 0 {
			t.Fatal("the reports share their slices with the manager")
		}
	}
	// The oldest reports were dropped, not the newest.
	if manager.LastReport().MessagesBefore == 0 {
		t.Fatal("the last report was not retained")
	}
}

func TestEstimatorCountsDenseScriptsHeavily(t *testing.T) {
	estimator := NewHeuristicEstimator()
	ascii := strings.Repeat("a", 400)
	dense := strings.Repeat("中", 400)
	// A dense script is worth far more per character, so treating it as cheap
	// would send an oversized request to a provider that rejects it.
	if estimator.EstimateText(dense) <= estimator.EstimateText(ascii) {
		t.Fatalf("dense %d, ascii %d", estimator.EstimateText(dense), estimator.EstimateText(ascii))
	}
	if estimator.EstimateText("") != 0 {
		t.Fatal("an empty string was not free")
	}
	// A short string still costs something, rather than rounding down to zero.
	if estimator.EstimateText("hi") < 1 {
		t.Fatal("a short string cost nothing")
	}
	// The structural cost of a message is counted on top of its text.
	if estimator.Estimate([]core.Message{user("hello")}) <= estimator.EstimateText("hello") {
		t.Fatal("message overhead was not counted")
	}
	// An image is worth something even though it has no text.
	image := core.Message{Content: []core.ContentBlock{{Type: core.ContentTypeImage}}}
	if estimator.Estimate([]core.Message{image}) < ImageTokenEstimate {
		t.Fatalf("an image cost %d", estimator.Estimate([]core.Message{image}))
	}
	// Every block type is counted, so an unusual one cannot hide cost.
	odd := core.Message{Content: []core.ContentBlock{{Type: "unheard_of", Text: "abc"}}}
	if estimator.Estimate([]core.Message{odd}) <= 0 {
		t.Fatal("an unknown block type cost nothing")
	}
}

func TestCJKRanges(t *testing.T) {
	for _, character := range []rune{'中', 'あ', 'ア', '한', '，', 'Ａ'} {
		if !IsCJK(character) {
			t.Fatalf("%q was not recognised as dense", character)
		}
	}
	for _, character := range []rune{'a', 'Z', '1', ' ', '€', 'Ω'} {
		if IsCJK(character) {
			t.Fatalf("%q was wrongly treated as dense", character)
		}
	}
}

func TestEstimateHonoursACustomEstimator(t *testing.T) {
	// A caller who knows the model's tokenizer supplies it, and the manager uses
	// it for every decision rather than its own.
	manager := newTestManager(t, Config{Estimator: fixedEstimator{perCall: 1000}})
	// Four messages at a thousand tokens each, counted by the caller's
	// estimator rather than by the manager's own.
	if manager.Estimate(conversation(2)) != 4000 {
		t.Fatalf("estimate = %d", manager.Estimate(conversation(2)))
	}
}

type fixedEstimator struct{ perCall int }

func (f fixedEstimator) EstimateText(string) int { return f.perCall }

func (f fixedEstimator) Estimate(messages []core.Message) int { return f.perCall * len(messages) }

func TestTruncateStrategyKeepsTheTail(t *testing.T) {
	messages := conversation(10)
	plan := CompactPlan{Cut: 4, KeepFrom: 4}
	compacted, err := TruncateStrategy{}.Compact(context.Background(), plan, messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(compacted) != len(messages)-4 {
		t.Fatalf("compacted = %d", len(compacted))
	}
	// A cut past the end is clamped rather than panicking, because a plan built
	// from a stale estimate is a caller bug that should not take the process down.
	compacted, err = TruncateStrategy{}.Compact(context.Background(), CompactPlan{Cut: 999}, messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(compacted) != 0 {
		t.Fatalf("compacted = %d", len(compacted))
	}
	// A cut of zero or less is a no op.
	compacted, err = TruncateStrategy{}.Compact(context.Background(), CompactPlan{Cut: 0}, messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(compacted) != len(messages) {
		t.Fatalf("compacted = %d", len(compacted))
	}
}
