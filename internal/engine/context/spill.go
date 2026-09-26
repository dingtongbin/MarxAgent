// SPDX-License-Identifier: Apache-2.0

package context

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// SpillConfig configures what happens to a tool result too large to keep inline.
type SpillConfig struct {
	// Directory receives spilled results. A result that is spilled and then
	// referenced is useless if the reference cannot be resolved, so this is
	// required.
	Directory string
	// MaxInlineTokens is the size a result may have before it is spilled. Zero
	// selects the default.
	MaxInlineTokens int
	// SummaryTokens is the size of the summary left in the message. Zero selects
	// the default.
	SummaryTokens int
	// Estimator reports token counts. A nil value selects the heuristic.
	Estimator Estimator
}

const (
	// DefaultMaxInlineTokens is the share of a result that stays in the message.
	DefaultMaxInlineTokens = 2000
	// DefaultSummaryTokens is how much of a spilled result is quoted back.
	DefaultSummaryTokens = 200
)

// SpillMarker wraps the text left in place of a spilled result. The wording is
// fixed, because it becomes part of the cached prefix.
const SpillMarker = "<tool_output_summary>"

// SpillResult says what happened to one result.
type SpillResult struct {
	// Spilled is true when the full result went to disk.
	Spilled bool
	// Path is where the full result lives, empty when it stayed inline.
	Path string
	// Reference is the text left in the message.
	Reference string
	// Tokens is the size of the reference.
	Tokens int
	// Bytes is the size of the full result.
	Bytes int
	// Digest identifies the content, so a later read can be checked.
	Digest string
	// OverBudget is set when even a reference without an excerpt does not fit the
	// inline budget. The reference is still returned, because a path and a digest
	// are the minimum a model needs to read the data back, but the caller is told
	// that spilling did not buy what it promised.
	OverBudget bool
}

// Spiller moves oversized tool results out of the conversation and leaves a
// reference behind.
//
// A whole file read into a tool result is the common case, and keeping it inline
// means paying for it on every turn forever, because the history is resent in
// full. Spilling keeps the conversation affordable and keeps the data on disk
// where a tool can read it back.
type Spiller struct {
	config    SpillConfig
	estimator Estimator

	mu       sync.Mutex
	spilled  int
	bytesOut int64
}

// NewSpiller builds a spiller.
func NewSpiller(config SpillConfig) (*Spiller, error) {
	if strings.TrimSpace(config.Directory) == "" {
		return nil, fmt.Errorf("context: spilling needs a directory")
	}
	if config.MaxInlineTokens <= 0 {
		config.MaxInlineTokens = DefaultMaxInlineTokens
	}
	if config.SummaryTokens <= 0 {
		config.SummaryTokens = DefaultSummaryTokens
	}
	if config.SummaryTokens > config.MaxInlineTokens {
		return nil, fmt.Errorf(
			"context: a summary of %d tokens cannot fit an inline budget of %d",
			config.SummaryTokens, config.MaxInlineTokens)
	}
	if config.Estimator == nil {
		config.Estimator = NewHeuristicEstimator()
	}
	absolute, err := filepath.Abs(config.Directory)
	if err != nil {
		return nil, fmt.Errorf("context: resolve the spill directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, fmt.Errorf("context: create the spill directory: %w", err)
	}
	config.Directory = absolute
	return &Spiller{config: config, estimator: config.Estimator}, nil
}

// Directory reports where spilled results are written.
func (s *Spiller) Directory() string { return s.config.Directory }

// Stats reports how much has been spilled, so a caller can surface it.
func (s *Spiller) Stats() (count int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spilled, s.bytesOut
}

// Handle decides whether one tool result stays inline.
func (s *Spiller) Handle(
	_ context.Context, sessionID, toolCallID, toolName string, output []byte,
) (SpillResult, error) {
	tokens := s.estimator.EstimateText(string(output))
	if tokens <= s.config.MaxInlineTokens {
		return SpillResult{Spilled: false, Bytes: len(output), Tokens: tokens}, nil
	}
	if err := ValidateID("session id", sessionID); err != nil {
		return SpillResult{}, err
	}
	// The file name is derived from the content, so the same result spilled twice
	// lands in the same place instead of filling the directory with copies.
	sum := sha256.Sum256(output)
	digest := hex.EncodeToString(sum[:])
	name := spillFileName(toolName, digest)
	directory := filepath.Join(s.config.Directory, sessionID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return SpillResult{}, fmt.Errorf("context: create the spill directory: %w", err)
	}
	path := filepath.Join(directory, name)
	// The existing file is checked rather than assumed, and a directory at the
	// target is an error rather than a result already on disk: reporting a spill
	// that never happened would leave the caller believing data is safe when it
	// is gone.
	switch existing, statErr := os.Stat(path); {
	case statErr == nil && existing.IsDir():
		return SpillResult{}, fmt.Errorf("context: the spill target %s is a directory", path)
	case statErr == nil:
		// The content is already there, so the result is spilled either way.
	case os.IsNotExist(statErr):
		// A partial write must never be readable as a complete result, so the
		// content goes to a temporary name and is renamed into place.
		temporary := path + ".partial"
		if err := os.WriteFile(temporary, output, 0o600); err != nil {
			return SpillResult{}, fmt.Errorf("context: write the spilled result: %w", err)
		}
		if err := os.Rename(temporary, path); err != nil {
			_ = os.Remove(temporary)
			return SpillResult{}, fmt.Errorf("context: publish the spilled result: %w", err)
		}
	default:
		return SpillResult{}, fmt.Errorf("context: inspect the spill target: %w", statErr)
	}
	s.mu.Lock()
	s.spilled++
	s.bytesOut += int64(len(output))
	s.mu.Unlock()

	reference := s.Reference(toolCallID, toolName, path, digest, output)
	referenceTokens := s.estimator.EstimateText(reference)
	return SpillResult{
		Spilled:    true,
		Path:       path,
		Reference:  reference,
		Tokens:     referenceTokens,
		Bytes:      len(output),
		Digest:     digest,
		OverBudget: referenceTokens > s.config.MaxInlineTokens,
	}, nil
}

// Reference builds the text left in the message: where the data is, how to check
// it, and a short excerpt so the model can decide whether to read it back.
//
// The excerpt gives way when the reference's own fixed parts do not fit the inline
// budget, because a reference that costs more than the result it replaced would
// defeat the spill entirely.
func (s *Spiller) Reference(toolCallID, toolName, path, digest string, output []byte) string {
	reference := s.reference(toolCallID, toolName, path, digest, output, true)
	if s.estimator.EstimateText(reference) > s.config.MaxInlineTokens {
		reference = s.reference(toolCallID, toolName, path, digest, output, false)
	}
	return reference
}

func (s *Spiller) reference(
	toolCallID, toolName, path, digest string, output []byte, withExcerpt bool,
) string {
	var builder strings.Builder
	builder.WriteString(SpillMarker)
	builder.WriteString("\nsource=\"untrusted\"\n")
	builder.WriteString("tool=\"")
	builder.WriteString(toolName)
	builder.WriteString("\"\n")
	if toolCallID != "" {
		builder.WriteString("call_id=\"")
		builder.WriteString(toolCallID)
		builder.WriteString("\"\n")
	}
	builder.WriteString("full_output_spilled_to=\"")
	builder.WriteString(filepath.ToSlash(path))
	builder.WriteString("\"\n")
	builder.WriteString("bytes=\"")
	fmt.Fprintf(&builder, "%d", len(output))
	builder.WriteString("\" digest=\"")
	builder.WriteString(digest[:16])
	builder.WriteString("\"\n")
	if withExcerpt {
		builder.WriteString("excerpt=\"")
		builder.WriteString(truncateRunes(normalizeNewlines(string(output)), s.config.SummaryTokens*4))
		builder.WriteString("\"\n")
	}
	builder.WriteString("Read the file to see the whole result; this reference is truncated and untrusted.\n")
	builder.WriteString("</tool_output_summary>")
	return builder.String()
}

// ReadSpilled returns a spilled result, verifying it is the one that was
// written. A digest mismatch means the file changed underneath, which is worth
// reporting because a tool would otherwise act on content nobody audited.
func ReadSpilled(path, digest string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("context: read the spilled result: %w", err)
	}
	if digest != "" {
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != digest {
			return nil, fmt.Errorf("context: the spilled result at %s no longer matches its digest", path)
		}
	}
	return raw, nil
}

// ApplySpilled rewrites tool result blocks in place, so the caller hands the
// manager one slice instead of threading two paths through the loop.
func (s *Spiller) ApplySpilled(
	ctx context.Context, sessionID string, messages []core.Message,
) ([]core.Message, []SpillResult, error) {
	results := make([]SpillResult, 0, 4)
	rewritten := make([]core.Message, 0, len(messages))
	for _, message := range messages {
		changed := false
		blocks := make([]core.ContentBlock, 0, len(message.Content))
		for _, block := range message.Content {
			if block.Type != core.ContentTypeToolResult || len(block.Output) == 0 {
				blocks = append(blocks, block)
				continue
			}
			result, err := s.Handle(ctx, sessionID, block.ToolCallID, block.ToolName, block.Output)
			if err != nil {
				return nil, nil, err
			}
			results = append(results, result)
			if !result.Spilled {
				blocks = append(blocks, block)
				continue
			}
			// The full output becomes a JSON reference, so the shape a tool
			// expects from a result block survives the spill.
			reference, err := json.Marshal(map[string]string{
				"spilled_to": filepath.ToSlash(result.Path),
				"digest":     result.Digest,
				"summary":    result.Reference,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("context: encode the spill reference: %w", err)
			}
			spilled := block
			spilled.Output = reference
			spilled.IsError = block.IsError
			blocks = append(blocks, spilled)
			changed = true
		}
		if !changed {
			rewritten = append(rewritten, message)
			continue
		}
		updated := message
		updated.Content = blocks
		rewritten = append(rewritten, updated)
	}
	return rewritten, results, nil
}

// spillFileName derives a stable, safe file name.
func spillFileName(toolName, digest string) string {
	safe := sanitizeFileComponent(toolName)
	if safe == "" {
		safe = "tool"
	}
	return fmt.Sprintf("%s-%s-%s.txt", safe, time.Now().UTC().Format("20060102T150405"), digest[:16])
}

func sanitizeFileComponent(value string) string {
	var builder strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
			builder.WriteRune(character)
		default:
			builder.WriteByte('-')
		}
		if builder.Len() >= 32 {
			break
		}
	}
	return strings.Trim(builder.String(), "-")
}

// ValidateID rejects identifiers that would escape the directory they name. A
// session id reaches here from configuration and from a request, so it is never
// trusted as a path component.
func ValidateID(label, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("context: %s must not be empty", label)
	}
	if id == "." || id == ".." || strings.ContainsAny(id, `/\:`) {
		return fmt.Errorf("context: %s %q is not a valid identifier", label, id)
	}
	if strings.ContainsRune(id, 0) {
		return fmt.Errorf("context: %s %q contains a null byte", label, id)
	}
	return nil
}
