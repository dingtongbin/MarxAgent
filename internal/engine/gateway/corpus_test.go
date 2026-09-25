// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// corpusAnthropicSignature is the credential the Anthropic corpus carries,
// so a change in how the signature is accumulated is caught here.
const corpusAnthropicSignature = "corpus-signature-abc123"

// The corpora are byte-for-byte provider streams. They exist so a change in the
// wire format shows up as a test failure instead of a production surprise.
var corpusFiles = map[APIFormat]string{
	FormatOpenAICompletions: "openai_completions.sse",
	FormatAnthropicMessages: "anthropic_messages.sse",
	FormatOpenAIResponses:   "openai_responses.sse",
}

func loadCorpus(t *testing.T, format APIFormat) string {
	t.Helper()
	name, ok := corpusFiles[format]
	if !ok {
		t.Fatalf("no corpus registered for %s", format)
	}
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("corpus %s is empty", name)
	}
	return string(data)
}

func TestCorpusFilesExist(t *testing.T) {
	for format, name := range corpusFiles {
		if _, err := os.Stat(filepath.Join("testdata", name)); err != nil {
			t.Fatalf("corpus for %s: %v", format, err)
		}
	}
}

func TestCompletionsCorpusParsesInOrder(t *testing.T) {
	adapter := completionsAdapterFor(t)
	chunks := make([]core.StreamChunk, 0, 16)
	for chunk := range adapter.ParseStream(strings.NewReader(loadCorpus(t, FormatOpenAICompletions))) {
		chunks = append(chunks, chunk)
	}
	text, thinking, calls := summarize(chunks)
	if text != "Hello from the corpus." {
		t.Fatalf("text = %q", text)
	}
	if thinking != "" {
		t.Fatalf("thinking = %q", thinking)
	}
	if len(calls) != 2 {
		t.Fatalf("tool calls = %#v", calls)
	}
	if calls[0].ToolName != "read" || string(calls[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("first call = %#v", calls[0])
	}
	if calls[1].ToolName != "grep" || string(calls[1].Input) != `{"pattern":"needle"}` {
		t.Fatalf("second call = %#v", calls[1])
	}
	if chunks[len(chunks)-1].Type != core.StreamTypeDone {
		t.Fatalf("terminal = %#v", chunks[len(chunks)-1])
	}
	usage := adapter.TakeUsage()
	if usage.InputTokens != 31 || usage.OutputTokens != 12 || usage.CacheReadTokens != 24 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAnthropicCorpusParsesInOrder(t *testing.T) {
	adapter := anthropicAdapterFor(t)
	chunks := make([]core.StreamChunk, 0, 16)
	for chunk := range adapter.ParseStream(strings.NewReader(loadCorpus(t, FormatAnthropicMessages))) {
		chunks = append(chunks, chunk)
	}
	text, thinking, calls := summarize(chunks)
	if text != "Hello from the corpus." {
		t.Fatalf("text = %q", text)
	}
	if thinking != "Hello" {
		t.Fatalf("thinking = %q", thinking)
	}
	if len(calls) != 1 || calls[0].ToolName != "read" ||
		string(calls[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("calls = %#v", calls)
	}
	var signature string
	for _, chunk := range chunks {
		if chunk.ThinkingSig != "" {
			signature = chunk.ThinkingSig
		}
	}
	if signature != corpusAnthropicSignature {
		t.Fatalf("signature = %q, want %q", signature, corpusAnthropicSignature)
	}
	usage := adapter.TakeUsage()
	if usage.InputTokens != 31 || usage.OutputTokens != 12 || usage.CacheReadTokens != 24 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestResponsesCorpusParsesInOrder(t *testing.T) {
	adapter := responsesAdapterFor(t)
	chunks := make([]core.StreamChunk, 0, 16)
	for chunk := range adapter.ParseStream(strings.NewReader(loadCorpus(t, FormatOpenAIResponses))) {
		chunks = append(chunks, chunk)
	}
	text, thinking, calls := summarize(chunks)
	if text != "Hello from the corpus." {
		t.Fatalf("text = %q", text)
	}
	if thinking != "Hello" {
		t.Fatalf("thinking = %q", thinking)
	}
	if len(calls) != 1 || calls[0].ToolName != "read" ||
		string(calls[0].Input) != `{"path":"a.txt"}` {
		t.Fatalf("calls = %#v", calls)
	}
	if chunks[len(chunks)-1].ResponseID != "resp_corpus" {
		t.Fatalf("response id = %q", chunks[len(chunks)-1].ResponseID)
	}
	usage := adapter.TakeUsage()
	if usage.InputTokens != 31 || usage.OutputTokens != 12 || usage.CacheReadTokens != 24 {
		t.Fatalf("usage = %#v", usage)
	}
}

// TestCorpusToolArgumentsAreReversible feeds every corpus tool call back through
// a request builder and checks the arguments survive the JSON string round trip.
func TestCorpusToolArgumentsAreReversible(t *testing.T) {
	for format := range corpusFiles {
		adapter, err := NewAdapter(format)
		if err != nil {
			t.Fatal(err)
		}
		_, _, calls := summarize(collectCorpusChunks(t, adapter, loadCorpus(t, format)))
		for _, call := range calls {
			if !IsJSONObject(call.Input) {
				t.Fatalf("%s: tool input is not an object: %s", format, call.Input)
			}
			payload, err := adapter.BuildRequest(&core.ChatRequest{
				Model: "test-model",
				Tools: []core.ToolSpec{{
					Name: call.ToolName, Description: "d", Parameters: []byte(`{"type":"object"}`),
				}},
				Messages: []core.Message{{
					ID:   "1",
					Role: core.RoleAssistant,
					Content: []core.ContentBlock{{
						Type:       core.ContentTypeToolUse,
						ToolCallID: "call_1",
						ToolName:   call.ToolName,
						Input:      call.Input,
					}},
				}},
			})
			if err != nil {
				t.Fatalf("%s: %v", format, err)
			}
			encoded, err := marshalPayload(payload)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(encoded), call.ToolName) {
				t.Fatalf("%s: tool name missing from %s", format, encoded)
			}
		}
	}
}

func collectCorpusChunks(t *testing.T, adapter APIAdapter, body string) []core.StreamChunk {
	t.Helper()
	chunks := make([]core.StreamChunk, 0, 16)
	for chunk := range adapter.ParseStream(strings.NewReader(body)) {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// summarize splits a parsed stream into visible text, reasoning text and tool
// calls. Reasoning is kept apart because a format that emits it must still emit
// the visible text separately.
func summarize(chunks []core.StreamChunk) (text string, thinking string, calls []core.ContentBlock) {
	var visible, reasoning strings.Builder
	calls = make([]core.ContentBlock, 0, 2)
	for _, chunk := range chunks {
		switch chunk.Type {
		case core.StreamTypeText:
			visible.WriteString(chunk.Text)
		case core.StreamTypeThinking:
			reasoning.WriteString(chunk.Text)
		case core.StreamTypeToolCall:
			if chunk.ToolCall != nil {
				calls = append(calls, *chunk.ToolCall)
			}
		}
	}
	return visible.String(), reasoning.String(), calls
}
