// SPDX-License-Identifier: Apache-2.0

package security

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// Gate adapts a Policy to the core.HookToolCallReceived hook, so a mode can gate
// tool calls with a hook rather than with code woven into the loop.
//
// The answer keeps every call in the list it was handed, because the assistant
// message has already been appended with one tool_use block per call and every
// provider format requires one result per block: a call a policy refuses is
// marked refused in place rather than dropped. Keeping the block also keeps its
// ToolCallID, which is what lets the loop recognize the call it already has and
// use the refusal's words as the result the model reads.
func Gate(policy Policy) core.HookFunc {
	if policy == nil {
		// A nil policy would be skipped by every caller that checks, and would
		// panic in every caller that does not. A gate that refuses everything is
		// the honest rendering of "no policy was assembled".
		policy = NoTools{}
	}
	return func(ctx context.Context, data any) (any, error) {
		calls, ok := data.([]core.ContentBlock)
		if !ok {
			return data, fmt.Errorf("security: gate received %T, want []core.ContentBlock", data)
		}
		answered := make([]core.ContentBlock, len(calls))
		for index, call := range calls {
			answered[index] = call
			if call.Type != core.ContentTypeToolUse || call.IsError {
				// A block that is not a call is a broken hook's problem, reported
				// by the loop's validation, and a call already marked refused
				// carries an answer of its own.
				continue
			}
			decision := policy.Evaluate(extractCall(call))
			if decision.Allowed && !decision.RequiresApproval {
				continue
			}
			answered[index] = refuseCall(call, decision)
		}
		return answered, nil
	}
}

// refuseCall rewrites one call into its refusal, keeping the shape the loop
// expects: the same tool_use block, the same ID, and an output that is the
// reason as a JSON string, which is the form the loop recovers reasons from.
func refuseCall(call core.ContentBlock, decision Decision) core.ContentBlock {
	refused := call
	refused.IsError = true
	refused.Output = json.RawMessage(strconv.Quote(refusalReason(decision)))
	return refused
}

// refusalReason renders a decision into the words the model reads. A refusal
// without a reason is indistinguishable from a fault, so an empty reason falls
// back to the rule that decided, and an approval the assembly cannot grant is
// said as that rather than left to look like an arbitrary refusal.
func refusalReason(decision Decision) string {
	reason := decision.Reason
	if reason == "" {
		reason = "refused by " + decision.Rule
	}
	if decision.RequiresApproval {
		reason += " (approval is not available in this build, so the call was refused)"
	}
	return reason
}

// extractCall narrows a content block to the part of a call a policy needs.
//
// The parameters stay raw JSON because a gate cannot know a caller's parameter
// struct, and the two fields every mode's policy reads — a filesystem path and
// a shell command — are lifted out of the common spellings of those names. A
// tool that spells its parameters differently is judged on its name alone,
// which is the policy author's problem to state rather than this gate's to
// guess at.
func extractCall(block core.ContentBlock) Call {
	call := Call{
		ToolName:   block.ToolName,
		Parameters: string(block.Input),
	}
	if len(block.Input) == 0 {
		return call
	}
	var fields struct {
		Path    string `json:"path"`
		Command string `json:"command"`
	}
	if err := json.Unmarshal(block.Input, &fields); err != nil {
		// Parameters that are not an object with those fields are not an error:
		// the call is simply one the gate judges without a path or a command.
		return call
	}
	call.Path = fields.Path
	call.Command = fields.Command
	return call
}

// Wrap adapts the untrusted envelope to the core.HookToolResult hook, so a mode
// can wrap every tool result without the loop knowing the envelope exists.
//
// The wrapped output is the JSON string form WrapToolResultJSON produces,
// because a result is carried as a json.RawMessage into the event stream, the
// transcript and the journal, and an envelope that is not JSON is a record that
// cannot be written down. The loop validates the result after this hook runs,
// so a wrap that produced invalid JSON would fail the turn here rather than
// silently pass a broken record downstream.
func Wrap() core.HookFunc {
	return func(ctx context.Context, data any) (any, error) {
		result, ok := data.(core.ToolResult)
		if !ok {
			return data, fmt.Errorf("security: wrap received %T, want core.ToolResult", data)
		}
		wrapped, err := WrapToolResultJSON(result.Output)
		if err != nil {
			return data, fmt.Errorf("security: wrap the tool result: %w", err)
		}
		result.Output = wrapped
		return result, nil
	}
}
