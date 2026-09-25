// SPDX-License-Identifier: Apache-2.0

package gateway

import "fmt"

// The three breakpoint slots the Anthropic format allows us to fill. A fourth is
// always available for a future use, and dynamic content is never given a
// boundary at all: it belongs to one turn only.
const (
	breakpointSystem = iota
	breakpointTools
	breakpointHistory
	breakpointSlots
)

type anthropicCacheHintMapper struct{}

// ApplyCacheHints places explicit cache breakpoints on an Anthropic payload. The
// slots are filled in descending order of value, so an over-subscribed request
// keeps the boundaries that pay off across turns instead of failing.
func (anthropicCacheHintMapper) ApplyCacheHints(payload any, hints CacheHints) error {
	converted, ok := payload.(*anthropicRequest)
	if !ok {
		return fmt.Errorf("%w: anthropic payload is %T", ErrInvalidGateway, payload)
	}
	limit := hints.MaxBreakpoints
	if limit <= 0 || limit > DefaultCacheBreakpoints {
		limit = DefaultCacheBreakpoints
	}

	wanted := make([]bool, breakpointSlots)
	if hints.StablePrefix {
		wanted[breakpointSystem] = true
		wanted[breakpointTools] = true
	}
	if hints.HistoryPrefix {
		wanted[breakpointHistory] = true
	}
	selected := selectBreakpoints(wanted, limit)

	applySystemBreakpoint(converted, selected[breakpointSystem])
	applyToolBreakpoint(converted, selected[breakpointTools])
	// Content injected for this turn must stay outside the cached prefix, so the
	// history boundary moves one message back whenever such content is present.
	applyHistoryBreakpoint(converted, selected[breakpointHistory], hints.DynamicTail)
	return nil
}

// selectBreakpoints keeps the highest value slots that fit and reports the rest
// as unselected, so a caller can see which boundary was dropped.
func selectBreakpoints(wanted []bool, limit int) []bool {
	selected := make([]bool, breakpointSlots)
	remaining := limit
	for slot := 0; slot < breakpointSlots && remaining > 0; slot++ {
		if !wanted[slot] {
			continue
		}
		selected[slot] = true
		remaining--
	}
	return selected
}

func ephemeral() *anthropicCacheControl {
	return &anthropicCacheControl{Type: "ephemeral"}
}

// applySystemBreakpoint promotes the system prompt from a plain string to a block
// list, because the format only accepts a cache boundary on a content block.
func applySystemBreakpoint(payload *anthropicRequest, selected bool) {
	switch system := payload.System.(type) {
	case string:
		if system == "" {
			return
		}
		blocks := []anthropicBlock{{Type: "text", Text: system}}
		if selected {
			blocks[0].CacheControl = ephemeral()
		}
		payload.System = blocks
	case []anthropicBlock:
		payload.System = clearBlockBreakpoints(system, selected)
	}
}

func applyToolBreakpoint(payload *anthropicRequest, selected bool) {
	for index := range payload.Tools {
		if selected {
			payload.Tools[index].CacheControl = ephemeral()
			return
		}
		payload.Tools[index].CacheControl = nil
	}
}

func applyHistoryBreakpoint(payload *anthropicRequest, selected bool, dynamicTail bool) {
	if len(payload.Messages) == 0 {
		return
	}
	target := len(payload.Messages) - 1
	if dynamicTail {
		target--
	}
	for index := range payload.Messages {
		payload.Messages[index].Content = clearBlockBreakpoints(
			payload.Messages[index].Content, selected && index == target)
	}
}

func clearBlockBreakpoints(blocks []anthropicBlock, last bool) []anthropicBlock {
	for index := range blocks {
		isLast := last && index == len(blocks)-1
		if isLast {
			blocks[index].CacheControl = ephemeral()
			continue
		}
		blocks[index].CacheControl = nil
	}
	return blocks
}

// ExtractCacheStats normalizes the Anthropic counters, which name cache reads
// and cache writes differently from every other format.
func (anthropicCacheHintMapper) ExtractCacheStats(usage Usage) CacheStats {
	stats := CacheStats{
		ReadTokens:  usage.CacheReadTokens,
		WriteTokens: usage.CacheWriteTokens,
		InputTokens: usage.InputTokens,
	}
	// The format reports the uncached portion of the prompt separately, so the
	// hit rate must be computed against the whole prompt.
	if total := usage.CacheReadTokens + usage.CacheWriteTokens; total > stats.InputTokens {
		stats.InputTokens = total
	}
	return stats
}

// BreakpointPlan describes where cache boundaries were placed, which the cache
// monitor uses to explain a low hit rate.
type BreakpointPlan struct {
	System      bool
	Tools       bool
	History     bool
	DynamicTail bool
	Dropped     int
}

// PlanBreakpoints reports the plan a hint set produces without touching a
// payload, so a caller can log it or assert on it.
func PlanBreakpoints(hints CacheHints) BreakpointPlan {
	limit := hints.MaxBreakpoints
	if limit <= 0 || limit > DefaultCacheBreakpoints {
		limit = DefaultCacheBreakpoints
	}
	wanted := []bool{hints.StablePrefix, hints.StablePrefix, hints.HistoryPrefix}
	selected := selectBreakpoints(wanted, limit)
	plan := BreakpointPlan{
		System:      selected[breakpointSystem],
		Tools:       selected[breakpointTools],
		History:     selected[breakpointHistory],
		DynamicTail: hints.DynamicTail,
	}
	for slot, want := range wanted {
		if want && !selected[slot] {
			plan.Dropped++
		}
	}
	return plan
}

// cacheControlCount reports how many explicit breakpoints a payload carries,
// which is how the tests and the monitor verify the format limit.
func cacheControlCount(payload *anthropicRequest) int {
	count := 0
	switch system := payload.System.(type) {
	case string:
		if system != "" {
			count++
		}
	case []anthropicBlock:
		for _, block := range system {
			if block.CacheControl != nil {
				count++
			}
		}
	}
	for index := range payload.Tools {
		if payload.Tools[index].CacheControl != nil {
			count++
		}
	}
	for _, message := range payload.Messages {
		for _, block := range message.Content {
			if block.CacheControl != nil {
				count++
			}
		}
	}
	return count
}
