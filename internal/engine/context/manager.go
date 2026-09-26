// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// Config is everything the manager needs. Every threshold is a field so the
// assembly layer owns the policy and this package owns only the mechanics.
type Config struct {
	// MaxContextRatio is the share of the model window a conversation may
	// occupy before compaction. Zero selects the default.
	MaxContextRatio float64
	// ModelWindow is the model's context window in tokens. It is required,
	// because a ratio of what is unknowable is not a policy.
	ModelWindow int
	// KeepRecentTurns is how many recent turns survive compaction verbatim. Zero
	// selects the default.
	KeepRecentTurns int
	// MaxMessageTokens caps a single message. A message over the cap is refused
	// rather than passed on, because a provider would reject the whole request
	// and the turn would be lost. Zero disables the cap.
	MaxMessageTokens int
	// KeepKeyResults protects tool results inside the folded region.
	KeepKeyResults bool
	// Estimator reports token counts. A nil value selects the heuristic.
	Estimator Estimator
	// Strategy folds the older region away. A nil value selects truncation,
	// which is the choice that cannot fail.
	Strategy Strategy
	// CacheInvalidator is told whenever compaction has to reset the cached
	// prefix. A nil value means the caller has no cache to reset.
	CacheInvalidator CacheInvalidator
	// ResponseChainInvalidator is told when a provider that keeps conversation
	// state server side has to forget it, because a compacted history no longer
	// matches the chain the server holds.
	ResponseChainInvalidator ResponseChainInvalidator
}

// CacheInvalidator resets the cached prefix signature.
type CacheInvalidator interface {
	InvalidateCache(reason string)
}

// CacheInvalidatorFunc adapts a function to CacheInvalidator.
type CacheInvalidatorFunc func(reason string)

// InvalidateCache calls the function.
func (f CacheInvalidatorFunc) InvalidateCache(reason string) { f(reason) }

// ResponseChainInvalidator makes a provider forget its server side conversation
// state.
type ResponseChainInvalidator interface {
	InvalidateResponseChain(reason string)
}

// ResponseChainInvalidatorFunc adapts a function to the interface.
type ResponseChainInvalidatorFunc func(reason string)

// InvalidateResponseChain calls the function.
func (f ResponseChainInvalidatorFunc) InvalidateResponseChain(reason string) { f(reason) }

const (
	// DefaultMaxContextRatio is the share of the window a conversation may use.
	DefaultMaxContextRatio = 0.7
	// DefaultKeepRecentTurns is how many recent turns compaction protects.
	DefaultKeepRecentTurns = 10
	// DefaultMaxMessageTokens caps a single message when the caller sets none.
	DefaultMaxMessageTokens = 20000
)

// ErrMessageTooLarge is returned when a single message cannot be admitted.
var ErrMessageTooLarge = errors.New("context: the message is larger than the configured cap")

// Report says what one compaction did, so a caller can log it and a test can
// assert on it without reaching into private state.
type Report struct {
	// TokensBefore and TokensAfter bracket the compaction.
	TokensBefore int
	TokensAfter  int
	// MessagesBefore and MessagesAfter bracket it.
	MessagesBefore int
	MessagesAfter  int
	// Dropped is how many messages the folded region held.
	Dropped int
	// Preserved lists the indexes inside the folded region that survived.
	Preserved []int
	// Strategy names the strategy that produced the result.
	Strategy string
	// FellBack is set when the chosen strategy failed and the guaranteed one
	// took over. It is the field worth alerting on.
	FellBack bool
	// FallbackReason explains a fallback.
	FallbackReason string
	// CacheInvalidated and ResponseChainInvalidated record the fallout.
	CacheInvalidated         bool
	ResponseChainInvalidated bool
	// StillOverBudget is set when even the newest messages do not fit, which
	// happens when one message alone exceeds the window. Nothing was forgotten to
	// report this: the single message cap is the defence for that case, and a
	// manager that quietly returned nothing would leave the next turn with no
	// context at all.
	StillOverBudget bool
	// Error is the strategy failure behind a fallback, kept for the log and not
	// returned, because degrading is better than failing the turn.
	Error error
}

// Manager decides when a conversation is too large and folds it down when it is.
type Manager struct {
	config    Config
	estimator Estimator
	strategy  Strategy
	fallback  Strategy

	mu      sync.Mutex
	last    Report
	history []Report
	// maxHistory bounds the retained reports so a long session cannot grow the
	// manager without limit.
	maxHistory int
}

// New builds a manager, applying the defaults for anything left unset.
func New(config Config) (*Manager, error) {
	if config.ModelWindow <= 0 {
		return nil, fmt.Errorf("context: the model window must be positive, got %d", config.ModelWindow)
	}
	if config.MaxContextRatio <= 0 || config.MaxContextRatio > 1 {
		config.MaxContextRatio = DefaultMaxContextRatio
	}
	if config.KeepRecentTurns <= 0 {
		config.KeepRecentTurns = DefaultKeepRecentTurns
	}
	if config.MaxMessageTokens < 0 {
		return nil, fmt.Errorf("context: the message cap must not be negative")
	}
	if config.Estimator == nil {
		config.Estimator = NewHeuristicEstimator()
	}
	if config.Strategy == nil {
		config.Strategy = TruncateStrategy{}
	}
	manager := &Manager{
		config:     config,
		estimator:  config.Estimator,
		strategy:   config.Strategy,
		fallback:   TruncateStrategy{},
		maxHistory: 64,
	}
	return manager, nil
}

// Config reports the configuration in force, with defaults applied.
func (m *Manager) Config() Config { return m.config }

// BudgetTokens is the number of tokens a conversation may occupy.
func (m *Manager) BudgetTokens() int {
	budget := int(float64(m.config.ModelWindow) * m.config.MaxContextRatio)
	if budget < 1 {
		budget = 1
	}
	return budget
}

// ShouldCompact reports whether a conversation has outgrown its budget.
func (m *Manager) ShouldCompact(messages []core.Message) bool {
	return m.estimator.Estimate(messages) > m.BudgetTokens()
}

// Estimate reports the tokens a conversation occupies.
func (m *Manager) Estimate(messages []core.Message) int {
	return m.estimator.Estimate(messages)
}

// LastReport returns the most recent compaction report.
func (m *Manager) LastReport() Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// Reports returns the retained compaction reports, oldest first.
func (m *Manager) Reports() []Report {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Report, 0, len(m.history))
	for _, report := range m.history {
		copied := report
		copied.Preserved = append([]int(nil), report.Preserved...)
		out = append(out, copied)
	}
	return out
}

// Admit checks a single message against the cap. The cap exists because a
// provider rejects the whole request when one message is too large, which loses
// the turn; refusing here loses only the message and says so.
func (m *Manager) Admit(messages []core.Message) ([]core.Message, error) {
	if m.config.MaxMessageTokens <= 0 {
		return messages, nil
	}
	admitted := make([]core.Message, 0, len(messages))
	for index, message := range messages {
		cost := m.estimator.Estimate([]core.Message{message})
		if cost > m.config.MaxMessageTokens {
			return admitted, fmt.Errorf("%w: message %d costs %d tokens, the cap is %d",
				ErrMessageTooLarge, index, cost, m.config.MaxMessageTokens)
		}
		admitted = append(admitted, message)
	}
	return admitted, nil
}

// Compact folds the older region away when the conversation is over budget. A
// conversation that is within budget is returned untouched and cheaply, because
// the common case must not pay for compaction.
func (m *Manager) Compact(
	ctx context.Context, messages []core.Message,
) ([]core.Message, Report, error) {
	before := m.estimator.Estimate(messages)
	report := Report{
		TokensBefore:   before,
		MessagesBefore: len(messages),
	}
	if before <= m.BudgetTokens() {
		report.TokensAfter = before
		report.MessagesAfter = len(messages)
		report.Strategy = "none"
		m.remember(report)
		return messages, report, nil
	}
	plan := m.Plan(messages, before)
	if plan.Cut <= 0 {
		// Nothing can be folded, either because there is nothing older than the
		// protected tail or because the window is smaller than the tail itself.
		// Hard truncation is the only honest move, and it is reported as such.
		report.Strategy = m.fallback.Name()
		report.FellBack = true
		report.FallbackReason = "nothing older than the protected tail could be folded"
		truncated, truncateReport := m.forceTruncate(messages, plan)
		report.TokensAfter = truncateReport
		report.MessagesAfter = len(truncated)
		report.Dropped = len(messages) - len(truncated)
		report.StillOverBudget = !m.fitsBudget(truncated)
		m.finish(&report)
		return truncated, report, nil
	}

	compacted, err := m.strategy.Compact(ctx, plan, messages)
	if err != nil {
		// Degrading is the whole point of having a fallback: a turn that loses
		// detail is recoverable, a turn that fails is not.
		compacted, fallbackReport := m.forceTruncate(messages, plan)
		report.Strategy = m.fallback.Name()
		report.FellBack = true
		report.FallbackReason = err.Error()
		report.Error = err
		report.TokensAfter = fallbackReport
		report.MessagesAfter = len(compacted)
		report.Dropped = len(messages) - len(compacted)
		report.StillOverBudget = !m.fitsBudget(compacted)
		m.finish(&report)
		return m.preserveKeyResults(messages, compacted, plan), report, nil
	}

	report.Strategy = m.strategy.Name()
	report.TokensAfter = m.estimator.Estimate(compacted)
	report.MessagesAfter = len(compacted)
	report.Dropped = plan.Cut
	report.Preserved = append(report.Preserved, plan.KeyResults...)
	// Compaction rewrote the prefix, so the cached signature is meaningless now
	// and the chain a server side provider holds no longer matches.
	m.finish(&report)
	return m.preserveKeyResults(messages, compacted, plan), report, nil
}

// preserveKeyResults puts back the tool results the plan marked as key.
//
// This lives in the manager rather than in a strategy because it is a property
// of the policy, not of the folding: a failed tool call has to survive whether
// the older region was summarized or simply dropped, otherwise the model loses
// the exact output it needed at the moment it needed it.
func (m *Manager) preserveKeyResults(
	original, compacted []core.Message, plan CompactPlan,
) []core.Message {
	if len(plan.KeyResults) == 0 {
		return compacted
	}
	kept := make(map[string]int)
	for index, message := range compacted {
		kept[messageIdentity(message)] = index
	}
	var missing []core.Message
	for _, index := range plan.KeyResults {
		if index < 0 || index >= len(original) {
			continue
		}
		if _, present := kept[messageIdentity(original[index])]; present {
			continue
		}
		missing = append(missing, original[index])
	}
	if len(missing) == 0 {
		return compacted
	}
	// They go in front of the tail, so the model reads the preserved output
	// before the recent turns it has to answer.
	result := make([]core.Message, 0, len(compacted)+len(missing))
	result = append(result, missing...)
	result = append(result, compacted...)
	return result
}

// messageIdentity identifies a message well enough to recognise it again after
// compaction has reordered things around it.
func messageIdentity(message core.Message) string {
	if message.ID != "" {
		return message.ID
	}
	first := ""
	if len(message.Content) > 0 {
		first = message.Content[0].Text
	}
	return string(message.Role) + "\x00" + first
}

// Plan decides where to cut. It is exported because the compaction hooks expose
// it, so a caller can log the decision before it happens.
func (m *Manager) Plan(messages []core.Message, tokens int) CompactPlan {
	plan := CompactPlan{
		TargetTokens:    m.BudgetTokens() / 4,
		BudgetTokens:    m.BudgetTokens(),
		KeepRecentTurns: m.config.KeepRecentTurns,
		Estimator:       m.estimator,
	}
	// A turn is a user message and everything that follows it, so the protected
	// tail is counted in turns rather than in messages.
	turnStarts := []int{0}
	for index, message := range messages {
		if message.Role == core.RoleUser {
			turnStarts = append(turnStarts, index)
		}
	}
	keepFrom := turnStarts[len(turnStarts)-1]
	if len(turnStarts) > 1 {
		keepFrom = turnStarts[maxInt(0, len(turnStarts)-1-m.config.KeepRecentTurns)]
	}
	plan.KeepFrom = keepFrom
	// Walk backwards from the protected tail while there is budget left, which
	// keeps as much verbatim history as the window allows rather than a fixed
	// count.
	cut := keepFrom
	for cut > 0 && m.estimator.Estimate(messages[cut-1:]) < m.BudgetTokens()-plan.TargetTokens {
		cut--
	}
	plan.Cut = cut
	if m.config.KeepKeyResults {
		for index := 0; index < cut; index++ {
			if isKeyResult(messages[index]) {
				plan.KeyResults = append(plan.KeyResults, index)
			}
		}
	}
	return plan
}

func (m *Manager) forceTruncate(messages []core.Message, plan CompactPlan) ([]core.Message, int) {
	cut := plan.Cut
	if cut <= 0 {
		// Nothing older than the tail could be folded, so the tail is all there
		// is. The cut walks forward to the first index whose suffix fits, which
		// keeps everything when everything fits and only the newest messages when
		// nothing does. Starting the walk from the end instead would drop the
		// whole conversation in the one case where it was about to be lost anyway.
		cut = 0
		for cut < len(messages) && m.estimator.Estimate(messages[cut:]) > m.BudgetTokens() {
			cut++
		}
		if cut >= len(messages) {
			// The newest message alone does not fit. It is kept anyway, because an
			// empty history costs the turn its context entirely, and the report
			// says the result is still over budget.
			cut = len(messages) - 1
		}
		if cut < 0 {
			cut = 0
		}
	}
	if cut > len(messages) {
		cut = len(messages)
	}
	kept := cloneMessages(messages[cut:])
	return kept, m.estimator.Estimate(kept)
}

// cloneMessages copies a slice of messages and their content blocks.
//
// The result of a compaction belongs to the caller, who will reasonably rewrite
// blocks in it. A shallow copy would let that reach back into the caller's own
// history, which is still the durable record.
func cloneMessages(messages []core.Message) []core.Message {
	if messages == nil {
		return nil
	}
	out := make([]core.Message, 0, len(messages))
	for _, message := range messages {
		out = append(out, cloneMessage(message))
	}
	return out
}

// fitsBudget reports whether a result is within the budget, so a report can say
// so plainly rather than leaving the caller to work it out.
func (m *Manager) fitsBudget(messages []core.Message) bool {
	return m.estimator.Estimate(messages) <= m.BudgetTokens()
}

func (m *Manager) finish(report *Report) {
	if report.Dropped > 0 || report.FellBack {
		reason := fmt.Sprintf("compaction folded %d messages using %s", report.Dropped, report.Strategy)
		if m.config.CacheInvalidator != nil {
			m.config.CacheInvalidator.InvalidateCache(reason)
			report.CacheInvalidated = true
		}
		if m.config.ResponseChainInvalidator != nil {
			// A provider holding the conversation server side has to forget it:
			// the history it holds no longer matches the one being sent.
			m.config.ResponseChainInvalidator.InvalidateResponseChain(reason)
			report.ResponseChainInvalidated = true
		}
	}
	m.remember(*report)
}

func (m *Manager) remember(report Report) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = report
	m.history = append(m.history, report)
	if len(m.history) > m.maxHistory {
		// Drop the oldest rather than the newest, so the recent behaviour is
		// what a caller inspecting the manager sees.
		m.history = m.history[len(m.history)-m.maxHistory:]
	}
}

// isKeyResult reports whether a message holds something worth keeping verbatim:
// a tool call that failed, or a result the caller marked.
func isKeyResult(message core.Message) bool {
	if message.Metadata != nil {
		if key, ok := message.Metadata["key_result"].(bool); ok && key {
			return true
		}
	}
	for _, block := range message.Content {
		if block.Type != core.ContentTypeToolResult {
			continue
		}
		if block.IsError {
			return true
		}
	}
	return false
}

func maxInt(first, second int) int {
	if first > second {
		return first
	}
	return second
}
