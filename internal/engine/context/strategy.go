// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"fmt"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// Strategy folds the older part of a conversation away.
//
// The interface exists because the choice is a policy, not a mechanic: summarizing
// through the model costs a round trip and reads better, truncating costs nothing
// and loses detail, and a mode with a small window may want the second while a
// mode with a long one wants the first. The assembly layer picks, this package
// supplies both and guarantees the fallback.
type Strategy interface {
	// Name identifies the strategy in logs and in the compaction report.
	Name() string
	// Compact replaces messages with a shorter equivalent history. The
	// implementation must return a slice whose first message is the summary and
	// whose remainder is the tail it chose to keep.
	Compact(ctx context.Context, plan CompactPlan, messages []core.Message) ([]core.Message, error)
}

// CompactPlan is what the manager decided, handed to the strategy so every
// strategy works from the same facts.
type CompactPlan struct {
	// Cut is the index of the first message kept verbatim. Everything before it
	// is the region to fold away.
	Cut int
	// KeepFrom is the index compaction starts from, which may be before Cut when
	// a tool result pair has to stay together.
	KeepFrom int
	// KeyResults lists indexes inside the folded region that must survive
	// verbatim, because a failed tool call or a marked result is what the model
	// needs to reason about.
	KeyResults []int
	// TargetTokens is the size the result should reach.
	TargetTokens int
	// BudgetTokens is the window the request must fit in.
	BudgetTokens int
	// KeepRecentTurns is how many recent turns the plan protects.
	KeepRecentTurns int
	// Estimator is the same estimator the manager used, so a strategy does not
	// have to be told how to count.
	Estimator Estimator
}

// SummarizeStrategy folds the older region into one summary message. The
// summary text is produced by Summarizer, which the assembly layer supplies
// because only it knows which model to spend the round trip on.
type SummarizeStrategy struct {
	// Summarizer produces the summary text. A nil summarizer makes this strategy
	// fail rather than silently degrade, so the manager can fall back on purpose
	// instead of by accident.
	Summarizer Summarizer
	// Header is the stable tag wrapped around the summary text. It is part of the
	// cached prefix once written, so it must not vary between runs.
	Header string
}

// Summarizer condenses text.
type Summarizer interface {
	Summarize(ctx context.Context, text string) (string, error)
}

// SummarizerFunc adapts a function to Summarizer.
type SummarizerFunc func(ctx context.Context, text string) (string, error)

// Summarize calls the function.
func (f SummarizerFunc) Summarize(ctx context.Context, text string) (string, error) {
	return f(ctx, text)
}

// Name identifies the strategy.
func (s *SummarizeStrategy) Name() string { return "summarize" }

// DefaultSummaryHeader wraps a summary so the model can tell it apart from user
// input. The wording is fixed because it becomes part of the cached prefix.
const DefaultSummaryHeader = "<conversation_summary>\n" +
	"Earlier turns of this conversation were compacted to stay inside the context window. " +
	"Treat the following as established history, not as a new request.\n"

// SummaryRole is the role a summary message carries.
//
// It is the user role on purpose. A system message would be hoisted into the
// system prompt by the provider gateway, which moves it into the cached prefix
// and invalidates the prefix on every compaction, and a summary that claims to
// be a model utterance would put words in the model's mouth. The user role is
// the only one that is legal in the middle of a conversation in all three API
// formats without either of those effects.
const SummaryRole = core.RoleUser

// SummaryMetadataKey marks a message as a compaction summary.
const SummaryMetadataKey = "marxagent.summary"

// Compact folds the older region into one summary message.
func (s *SummarizeStrategy) Compact(
	ctx context.Context, plan CompactPlan, messages []core.Message,
) ([]core.Message, error) {
	if s.Summarizer == nil {
		return nil, fmt.Errorf("context: the summarize strategy needs a summarizer")
	}
	if plan.Cut <= 0 {
		return cloneMessages(messages), nil
	}
	region := messages[:plan.Cut]
	summaryText, err := s.SummarizeRegion(ctx, region, plan)
	if err != nil {
		return nil, err
	}
	header := s.Header
	if header == "" {
		header = DefaultSummaryHeader
	}
	summary := core.Message{
		Role: SummaryRole,
		Content: []core.ContentBlock{{
			Type: core.ContentTypeText,
			Text: header + summaryText + "\n</conversation_summary>",
		}},
		Metadata: map[string]any{
			SummaryMetadataKey: true,
			"compacted":        plan.Cut,
		},
	}
	// The manager puts the key results back after the strategy runs, because
	// keeping them is a property of the policy rather than of the folding. The
	// tail is cloned so the result belongs to the caller.
	return append([]core.Message{summary}, cloneMessages(messages[plan.Cut:])...), nil
}

// SummarizeRegion condenses the folded region into text.
func (s *SummarizeStrategy) SummarizeRegion(
	ctx context.Context, region []core.Message, plan CompactPlan,
) (string, error) {
	var builder []byte
	for _, message := range region {
		for _, block := range message.Content {
			switch block.Type {
			case core.ContentTypeText:
				builder = append(builder, message.Role...)
				builder = append(builder, ": "...)
				builder = append(builder, block.Text...)
				builder = append(builder, '\n')
			case core.ContentTypeToolResult:
				builder = append(builder, "tool_result("...)
				builder = append(builder, block.ToolName...)
				builder = append(builder, "): "...)
				builder = append(builder, normalizeNewlines(string(block.Output))...)
				builder = append(builder, '\n')
			case core.ContentTypeToolUse:
				builder = append(builder, "tool_use("...)
				builder = append(builder, block.ToolName...)
				builder = append(builder, "): "...)
				builder = append(builder, string(block.Input)...)
				builder = append(builder, '\n')
			}
		}
	}
	text, err := s.Summarizer.Summarize(ctx, string(builder))
	if err != nil {
		return "", fmt.Errorf("context: summarize the folded region: %w", err)
	}
	if plan.TargetTokens > 0 && plan.Estimator != nil {
		// A summary that is not smaller than what it replaced has defeated the
		// purpose, so it is cut down rather than accepted.
		if plan.Estimator.EstimateText(text) > plan.TargetTokens {
			text = truncateRunes(text, plan.TargetTokens*4)
		}
	}
	return text, nil
}

// TruncateStrategy drops the folded region without summarizing it. It never
// fails, which makes it the fallback the manager uses when the chosen strategy
// does.
type TruncateStrategy struct{}

// Name identifies the strategy.
func (TruncateStrategy) Name() string { return "truncate" }

// Compact keeps the tail and records what was dropped.
func (TruncateStrategy) Compact(
	_ context.Context, plan CompactPlan, messages []core.Message,
) ([]core.Message, error) {
	if plan.Cut <= 0 {
		return cloneMessages(messages), nil
	}
	if plan.Cut > len(messages) {
		plan.Cut = len(messages)
	}
	return cloneMessages(messages[plan.Cut:]), nil
}

var (
	_ Strategy = (*SummarizeStrategy)(nil)
	_ Strategy = TruncateStrategy{}
)
