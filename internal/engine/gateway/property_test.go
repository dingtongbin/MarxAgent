// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// universalBuilder produces valid conversations from the superset, including the
// credential carrying blocks, so a round trip can be asserted on random input
// instead of a handful of hand written cases.
type universalBuilder struct {
	random *rand.Rand
}

func (b *universalBuilder) text() string {
	words := []string{"alpha", "beta", "gamma", "delta", "第", "一", "句"}
	length := 1 + b.random.IntN(4)
	out := ""
	for index := 0; index < length; index++ {
		out += words[b.random.IntN(len(words))]
	}
	return out
}

func (b *universalBuilder) toolName(index int) string {
	names := []string{"read", "write", "glob", "grep", "bash", "wiki_search", "lsp_hover"}
	return names[index%len(names)]
}

func (b *universalBuilder) object() []byte {
	return []byte(`{"path":"` + b.text() + `","limit":` + string(rune('0'+b.random.IntN(9))) + `}`)
}

func (b *universalBuilder) conversation(turns int) *core.ChatRequest {
	tools := make([]core.ToolSpec, 0, 7)
	for _, name := range []string{"read", "write", "glob", "grep", "bash", "wiki_search", "lsp_hover"} {
		tools = append(tools, core.ToolSpec{
			Name: name, Description: name + " tool.", Parameters: []byte(`{"type":"object"}`),
		})
	}
	request := &core.ChatRequest{
		Model:      "test-model",
		SystemHint: "be terse",
		Tools:      tools,
	}
	for turn := 0; turn < turns; turn++ {
		blocks := []core.ContentBlock{{Type: core.ContentTypeText, Text: b.text()}}
		if b.random.IntN(3) == 0 {
			blocks = append(blocks, core.ContentBlock{
				Type:        core.ContentTypeImage,
				ImageData:   "aGVsbG8=",
				MediaFormat: "png",
			})
		}
		request.Messages = append(request.Messages, core.Message{Role: core.RoleUser, Content: blocks})

		assistant := core.Message{Role: core.RoleAssistant}
		if b.random.IntN(2) == 0 {
			assistant.Content = append(assistant.Content, core.ContentBlock{Type: core.ContentTypeText, Text: b.text()})
		}
		if turn%2 == 0 {
			assistant.Content = append(assistant.Content, core.ContentBlock{
				Type:       core.ContentTypeToolUse,
				ToolCallID: "call_" + b.text(),
				ToolName:   b.toolName(turn),
				Input:      b.object(),
			})
			request.Messages = append(request.Messages, assistant)
			request.Messages = append(request.Messages, core.Message{
				Role: core.RoleTool,
				Content: []core.ContentBlock{{
					Type:       core.ContentTypeToolResult,
					ToolCallID: "call_" + b.text(),
					ToolName:   b.toolName(turn),
					Output:     []byte(`"` + b.text() + `"`),
				}},
			})
			continue
		}
		if len(assistant.Content) == 0 {
			assistant.Content = append(assistant.Content, core.ContentBlock{Type: core.ContentTypeText, Text: b.text()})
		}
		request.Messages = append(request.Messages, assistant)
	}
	return request
}

// credentials are the fields a conversion must never drop or rewrite.
type credentials struct {
	signature string
	encrypted string
	arguments string
}

func credentialsIn(request *core.ChatRequest) credentials {
	found := credentials{}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case core.ContentTypeThinking:
				found.signature += block.ThinkingSig
			case core.ContentTypeReasoning:
				found.encrypted += block.EncryptedContent
			case core.ContentTypeToolUse:
				found.arguments += string(block.Input)
			}
		}
	}
	return found
}

// TestPropertyConversionsKeepCredentials checks the invariant the whole gateway
// exists for: a format round trip never loses or rewrites a credential field.
func TestPropertyConversionsKeepCredentials(t *testing.T) {
	random := rand.New(rand.NewPCG(7, 11))
	builder := &universalBuilder{random: random}
	for iteration := 0; iteration < 200; iteration++ {
		request := builder.conversation(1 + random.IntN(4))
		// A thinking block with a signature is only replayable by Anthropic, and a
		// reasoning block only by Responses, so each format is exercised with the
		// blocks it actually supports.
		withSignature := *request
		withSignature.Messages = append([]core.Message(nil), request.Messages...)
		for index := range withSignature.Messages {
			if withSignature.Messages[index].Role != core.RoleAssistant {
				continue
			}
			blocks := append([]core.ContentBlock(nil), withSignature.Messages[index].Content...)
			blocks = append([]core.ContentBlock{{
				Type:        core.ContentTypeThinking,
				Thinking:    builder.text(),
				ThinkingSig: "sig-" + builder.text(),
			}}, blocks...)
			withSignature.Messages[index].Content = blocks
			break
		}
		withReasoning := *request
		withReasoning.Messages = append([]core.Message(nil), request.Messages...)
		for index := range withReasoning.Messages {
			if withReasoning.Messages[index].Role != core.RoleAssistant {
				continue
			}
			blocks := append([]core.ContentBlock(nil), withReasoning.Messages[index].Content...)
			blocks = append([]core.ContentBlock{{
				Type:             core.ContentTypeReasoning,
				EncryptedContent: "enc-" + builder.text(),
			}}, blocks...)
			withReasoning.Messages[index].Content = blocks
			break
		}

		t.Run(formatName(FormatAnthropicMessages, iteration), func(t *testing.T) {
			adapter := anthropicAdapterFor(t)
			payload, err := adapter.BuildRequest(&withSignature)
			if err != nil {
				t.Fatal(err)
			}
			converted, ok := payload.(*anthropicRequest)
			if !ok {
				t.Fatalf("payload = %T", payload)
			}
			signature := ""
			for _, message := range converted.Messages {
				for _, block := range message.Content {
					if block.Type == "thinking" {
						signature += block.Signature
					}
				}
			}
			want := credentialsIn(&withSignature).signature
			if signature != want {
				t.Fatalf("signature = %q, want %q", signature, want)
			}
			assertToolArgumentsSurvive(t, converted, &withSignature)
		})

		t.Run(formatName(FormatOpenAIResponses, iteration), func(t *testing.T) {
			adapter := responsesAdapterFor(t)
			payload, err := adapter.BuildRequest(&withReasoning)
			if err != nil {
				t.Fatal(err)
			}
			converted, ok := payload.(*responsesRequest)
			if !ok {
				t.Fatalf("payload = %T", payload)
			}
			encrypted := ""
			arguments := ""
			for _, item := range converted.Input {
				switch item.Type {
				case "reasoning":
					encrypted += item.EncryptedContent
				case "function_call":
					arguments += item.Arguments
				}
			}
			if want := credentialsIn(&withReasoning).encrypted; encrypted != want {
				t.Fatalf("encrypted content = %q, want %q", encrypted, want)
			}
			if want := credentialsIn(&withReasoning).arguments; arguments != want {
				t.Fatalf("arguments = %q, want %q", arguments, want)
			}
		})
	}
}

func formatName(format APIFormat, iteration int) string {
	return string(format) + "-case-" + strconv.Itoa(iteration)
}

func assertToolArgumentsSurvive(t *testing.T, payload *anthropicRequest, want *core.ChatRequest) {
	t.Helper()
	got := ""
	for _, message := range payload.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" {
				got += string(block.Input)
			}
		}
	}
	if expected := credentialsIn(want).arguments; got != expected {
		t.Fatalf("tool arguments = %q, want %q", got, expected)
	}
}
