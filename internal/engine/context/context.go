// SPDX-License-Identifier: Apache-2.0

// Package context holds the context management part: when a conversation grows
// past the share of a model window the design allows, how the older part is
// folded away, what happens to a tool result too large to keep inline, and how
// the pipeline defends itself when a single message is larger than any window.
//
// Nothing here decides anything for a mode. The strategy is an interface, the
// estimator is an interface, and every threshold arrives through Config, so the
// assembly layer picks the policy and this package only implements the mechanics.
package context

import (
	"strings"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// Estimator reports how many tokens a set of messages occupies.
//
// It is an interface because an exact count needs the model's own tokenizer,
// which is a property of the provider the assembly layer chose. The default
// implementation is an estimate, and it is deliberately honest about being one:
// it is used to decide when to compact, and compacting a little early is far
// cheaper than a request the provider rejects.
type Estimator interface {
	// EstimateText reports the tokens a string occupies.
	EstimateText(text string) int
	// Estimate reports the tokens a conversation occupies, including the
	// structural overhead of every message and block.
	Estimate(messages []core.Message) int
}

// PerMessageOverhead is the structural cost of a message beyond its text, in
// tokens: the role, the separators and the block framing that every provider
// adds on top of the content itself.
const PerMessageOverhead = 4

// PerBlockOverhead is the structural cost of one content block.
const PerBlockOverhead = 2

// HeuristicEstimator counts a CJK rune as one token and anything else as a
// quarter of one. That is close enough for a dense script to be treated as
// expensive rather than cheap, which is the error that matters: undercounting
// sends an oversized request, overcounting only compacts a little sooner.
type HeuristicEstimator struct {
	// ASCIIPerToken is how many non CJK runes make a token. Zero selects four.
	ASCIIPerToken int
	// CJKPerToken is how many CJK runes make a token. Zero selects one, because
	// a dense script is closer to one token per character.
	CJKPerToken int
}

// NewHeuristicEstimator returns the default estimator.
func NewHeuristicEstimator() *HeuristicEstimator { return &HeuristicEstimator{} }

// EstimateText reports the tokens a string occupies.
//
// The two scripts are counted separately and divided by their own densities.
// Accumulating them into one number and dividing once would make a dense script
// look cheap, which is the one direction that gets a request rejected by the
// provider rather than merely compacted a little early.
func (e *HeuristicEstimator) EstimateText(text string) int {
	perASCII := e.ASCIIPerToken
	if perASCII <= 0 {
		perASCII = 4
	}
	perCJK := e.CJKPerToken
	if perCJK <= 0 {
		perCJK = 1
	}
	dense, sparse := 0, 0
	for _, character := range text {
		if IsCJK(character) {
			dense++
			continue
		}
		sparse++
	}
	return ceilDiv(dense, perCJK) + ceilDiv(sparse, perASCII)
}

// ceilDiv divides without losing the remainder, because a string made of many
// small pieces would otherwise round down to nothing.
func ceilDiv(value, divisor int) int {
	if divisor <= 0 {
		return value
	}
	return (value + divisor - 1) / divisor
}

// Estimate reports the tokens a conversation occupies.
func (e *HeuristicEstimator) Estimate(messages []core.Message) int {
	total := 0
	for _, message := range messages {
		total += PerMessageOverhead
		for _, block := range message.Content {
			total += PerBlockOverhead
			total += e.estimateBlock(block)
		}
	}
	return total
}

func (e *HeuristicEstimator) estimateBlock(block core.ContentBlock) int {
	switch block.Type {
	case core.ContentTypeText:
		return e.EstimateText(block.Text)
	case core.ContentTypeThinking:
		return e.EstimateText(block.Thinking)
	case core.ContentTypeImage:
		// An image is billed by the provider on its own scale, and an estimate
		// that ignored it would compact a conversation carrying images for no
		// reason at all. This is a deliberate, documented guess.
		return ImageTokenEstimate
	case core.ContentTypeToolUse:
		return e.EstimateText(block.ToolName) + e.EstimateText(string(block.Input))
	case core.ContentTypeToolResult:
		return e.EstimateText(string(block.Output))
	case core.ContentTypeReasoning:
		return e.EstimateText(block.EncryptedContent)
	default:
		return e.EstimateText(block.Text)
	}
}

// ImageTokenEstimate is what an image is worth when the real figure is unknown.
// It is large enough that a conversation full of images compacts before the
// provider rejects it.
const ImageTokenEstimate = 1500

// IsCJK reports whether a rune belongs to a CJK range. Compact scripts are
// token dense, so the estimator has to know about them or an otherwise small
// conversation looks tiny.
func IsCJK(character rune) bool {
	switch {
	case character >= 0x4E00 && character <= 0x9FFF, // CJK unified ideographs
		character >= 0x3400 && character <= 0x4DBF, // extension A
		character >= 0x3040 && character <= 0x30FF, // kana
		character >= 0xAC00 && character <= 0xD7AF, // hangul syllables
		character >= 0xF900 && character <= 0xFAFF, // compatibility ideographs
		character >= 0x3000 && character <= 0x303F, // CJK punctuation
		character >= 0xFF00 && character <= 0xFFEF: // full width forms
		return true
	default:
		return false
	}
}

// estimateRunes is used by the spill summary, which only needs a length that
// scales rather than an exact count.
func estimateRunes(text string) int { return len([]rune(text)) }

// truncateRunes cuts a string to a rune budget without splitting a character.
func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// normalizeNewlines keeps a spill summary single line so a truncated result
// cannot break the line oriented journal it is quoted into.
func normalizeNewlines(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", " "), "\n", " ")
}
