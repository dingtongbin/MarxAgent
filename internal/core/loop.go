// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

func (a *agent) run(ctx context.Context, input Input, events chan<- Event) {
	defer close(events)
	defer a.running.Store(false)

	var runErr error
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				runErr = fmt.Errorf("core: agent loop panic: %v", recovered)
			}
		}()
		runErr = a.executeLoop(ctx, input, events)
	}()

	_, postLoopErr := a.hooks.trigger(ctx, HookPostLoop, a.State())
	if runErr == nil && postLoopErr != nil {
		runErr = postLoopErr
	}
	if runErr != nil {
		_ = a.emitError(ctx, events, "agent_loop", runErr)
		return
	}

	doneEvent, err := newEvent(EventTypeDone, DonePayload{State: a.state.snapshot()})
	if err != nil {
		_ = a.emitError(ctx, events, "done_event", err)
		return
	}
	a.setEventIdentity(&doneEvent)
	a.bus.Publish(ctx, doneEvent)
	select {
	case events <- doneEvent:
	case <-ctx.Done():
	}
}

func (a *agent) executeLoop(ctx context.Context, input Input, events chan<- Event) error {
	hooked, err := a.hooks.trigger(ctx, HookPreLoop, input)
	if err != nil {
		return err
	}
	input, ok := hooked.(Input)
	if !ok {
		return fmt.Errorf("core: pre_loop hook returned %T, want Input", hooked)
	}
	if err := validateInput(input); err != nil {
		return fmt.Errorf("core: pre_loop input: %w", err)
	}

	userMessage, err := a.newUserMessage(input)
	if err != nil {
		return err
	}
	a.state.appendMessage(userMessage)
	if err := a.emitStateChange(ctx, events, userMessage); err != nil {
		return err
	}

	for iteration := 0; iteration < a.config.MaxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		request, err := a.buildRequest(ctx)
		if err != nil {
			return err
		}
		messageID, err := generateUUID()
		if err != nil {
			return fmt.Errorf("core: generate assistant message ID: %w", err)
		}
		stream, err := a.provider.ChatStream(ctx, request)
		if err != nil {
			return fmt.Errorf("core: provider %q: %w", a.provider.Name(), err)
		}
		if stream == nil {
			return fmt.Errorf("core: provider %q returned a nil stream", a.provider.Name())
		}

		accumulator, streamErr := a.consumeStream(ctx, events, stream, messageID)
		if streamErr != nil {
			if message, exists := accumulator.message(messageID, false); exists {
				message.Incomplete = true
				a.state.appendMessage(message)
				if eventErr := a.emitStateChange(ctx, events, message); eventErr != nil {
					return errors.Join(streamErr, eventErr)
				}
			}
			return streamErr
		}

		assistantMessage, hasAssistantMessage := accumulator.message(messageID, false)
		if hasAssistantMessage {
			a.state.appendMessage(assistantMessage)
			if err := a.emitStreamCompletion(ctx, events, assistantMessage.ID); err != nil {
				return err
			}
			if err := a.emitStateChange(ctx, events, assistantMessage); err != nil {
				return err
			}
		}

		postModelValue, err := a.hooks.trigger(ctx, HookPostModelCall, request)
		if err != nil {
			return err
		}
		if _, ok := postModelValue.(ChatRequest); !ok {
			return fmt.Errorf("core: post_model_call hook returned %T, want ChatRequest", postModelValue)
		}
		toolCalls := accumulator.toolCalls()
		if len(toolCalls) == 0 {
			return nil
		}

		hookedCalls, err := a.hooks.trigger(ctx, HookToolCallReceived, toolCalls)
		if err != nil {
			return err
		}
		accepted, ok := hookedCalls.([]ContentBlock)
		if !ok {
			return fmt.Errorf("core: tool_call_received hook returned %T, want []ContentBlock", hookedCalls)
		}
		// The hook answered with the shape it was handed, so a block that is not a
		// call is a broken hook rather than a refusal, and it is reported as one. An
		// empty answer is the legitimate way to refuse everything.
		if err := validateToolCalls(accepted); err != nil {
			return fmt.Errorf("core: tool_call_received hook returned an invalid call: %w", err)
		}
		// A gate may refuse a call by dropping it from the list, but the assistant
		// message already carries its tool_use block and every one of the three API
		// formats requires an answer for each: a tool_use with no tool_result is a
		// rejected request, so a refusal that goes unanswered would fail the whole
		// turn on the next call rather than merely skipping the tool. The refused
		// calls are put back as failures carrying the reason the gate gave.
		toolCalls = reconcileRefusedCalls(toolCalls, accepted)
		if err := validateToolCalls(toolCalls); err != nil {
			return err
		}
		if err := a.emitToolCalls(ctx, events, assistantMessage.ID, toolCalls); err != nil {
			return err
		}

		results, toolErr := a.runTools(ctx, toolCalls)
		if len(results) > 0 {
			if err := a.emitToolResults(ctx, events, results); err != nil {
				return errors.Join(toolErr, err)
			}
			toolMessages, err := toolResultMessages(results)
			if err != nil {
				return errors.Join(toolErr, err)
			}
			a.state.appendMessages(toolMessages)
			for _, message := range toolMessages {
				if err := a.emitStateChange(ctx, events, message); err != nil {
					return errors.Join(toolErr, err)
				}
			}
		}
		if toolErr != nil {
			return toolErr
		}
	}
	return ErrMaxIterations
}

func (a *agent) buildRequest(ctx context.Context) (ChatRequest, error) {
	snapshot := a.state.snapshot()
	request := ChatRequest{
		Model:       a.config.Model,
		Messages:    snapshot.Messages,
		Tools:       cloneToolSpecs(a.toolSpecs),
		SystemHint:  a.config.SystemHint,
		MaxTokens:   a.config.MaxTokens,
		Temperature: a.config.Temperature,
	}
	hooked, err := a.hooks.trigger(ctx, HookPreModelCall, request)
	if err != nil {
		return ChatRequest{}, err
	}
	request, ok := hooked.(ChatRequest)
	if !ok {
		return ChatRequest{}, fmt.Errorf("core: pre_model_call hook returned %T, want ChatRequest", hooked)
	}
	hooked, err = a.hooks.trigger(ctx, HookContextInject, request)
	if err != nil {
		return ChatRequest{}, err
	}
	request, ok = hooked.(ChatRequest)
	if !ok {
		return ChatRequest{}, fmt.Errorf("core: context_inject hook returned %T, want ChatRequest", hooked)
	}
	return request, nil
}

func (a *agent) consumeStream(
	ctx context.Context,
	events chan<- Event,
	stream <-chan StreamChunk,
	messageID string,
) (*messageAccumulator, error) {
	accumulator := &messageAccumulator{}
	for {
		select {
		case <-ctx.Done():
			return accumulator, ctx.Err()
		case chunk, open := <-stream:
			if !open {
				return accumulator, nil
			}
			if err := a.emitPartial(ctx, events, messageID, chunk); err != nil {
				return accumulator, err
			}
			if err := accumulator.add(chunk); err != nil {
				return accumulator, err
			}
		}
	}
}

// reconcileRefusedCalls merges a gate's answer back into the calls the model made.
//
// A hook that rewrites a call has its version kept, because a gate may correct
// arguments as well as refuse. A hook that drops a call has it put back as a
// failure, because the assistant message has already been appended with that
// call in it and all three API formats require one result per call. A gate that
// wants to say why may keep the call and mark it refused with its own words, and
// that version is used as it stands.
func reconcileRefusedCalls(original, accepted []ContentBlock) []ContentBlock {
	byID := make(map[string]ContentBlock, len(accepted))
	for _, call := range accepted {
		byID[call.ToolCallID] = call
	}
	merged := make([]ContentBlock, 0, len(original))
	for _, call := range original {
		kept, exists := byID[call.ToolCallID]
		if !exists {
			refusal := call
			refusal.IsError = true
			refusal.Output = json.RawMessage(strconv.Quote(
				fmt.Sprintf("tool %q was refused by policy before it ran", call.ToolName)))
			kept = refusal
		}
		merged = append(merged, kept)
	}
	return merged
}

func (a *agent) runTools(ctx context.Context, calls []ContentBlock) ([]ToolResult, error) {
	// A call a gate already refused is not offered to a tool, but it still passes
	// through the execution hooks, because an attempt that was stopped is exactly
	// what an audit has to be able to show.
	refusals := make(map[int]ContentBlock)
	invocations := make([]ToolInvocation, len(calls))
	for index, call := range calls {
		invocations[index] = ToolInvocation{
			ToolCallID: call.ToolCallID,
			ToolName:   call.ToolName,
			Input:      cloneRawJSON(call.Input),
		}
		if call.IsError {
			refusals[index] = call
			continue
		}
		hooked, err := a.hooks.trigger(ctx, HookPreToolExec, invocations[index])
		if err != nil {
			return nil, err
		}
		invocation, ok := hooked.(ToolInvocation)
		if !ok {
			return nil, fmt.Errorf("core: pre_tool_exec hook returned %T, want ToolInvocation", hooked)
		}
		if len(invocation.Input) == 0 {
			invocation.Input = json.RawMessage(`{}`)
		}
		invocations[index] = invocation
		if err := validateToolInvocation(invocations[index]); err != nil {
			return nil, err
		}
	}

	results := a.executeToolsConcurrently(ctx, invocations, refusals)
	hooked, err := a.hooks.trigger(ctx, HookPostToolExec, results)
	if err != nil {
		return results, err
	}
	results, ok := hooked.([]ToolResult)
	if !ok {
		return results, fmt.Errorf("core: post_tool_exec hook returned %T, want []ToolResult", hooked)
	}
	if len(results) != len(invocations) {
		return results, fmt.Errorf("core: post_tool_exec hook returned %d results, want %d", len(results), len(invocations))
	}
	for index := range results {
		hooked, err = a.hooks.trigger(ctx, HookToolResult, results[index])
		if err != nil {
			return results, err
		}
		result, ok := hooked.(ToolResult)
		if !ok {
			return results, fmt.Errorf("core: tool_result hook returned %T, want ToolResult", hooked)
		}
		results[index] = result
		if err := validateToolResult(results[index]); err != nil {
			return results, err
		}
	}
	return results, nil
}

func (a *agent) executeToolsConcurrently(
	ctx context.Context,
	invocations []ToolInvocation,
	refusals map[int]ContentBlock,
) []ToolResult {
	results := make([]ToolResult, len(invocations))
	var waitGroup sync.WaitGroup
	waitGroup.Add(len(invocations))
	for index := range invocations {
		go func(resultIndex int) {
			defer waitGroup.Done()
			if refusal, stopped := refusals[resultIndex]; stopped {
				results[resultIndex] = refusedToolResult(invocations[resultIndex], refusal)
				return
			}
			results[resultIndex] = a.executeTool(ctx, invocations[resultIndex])
		}(index)
	}
	waitGroup.Wait()
	return results
}

// refusedToolResult reports a call a gate stopped, carrying the words the gate
// used. A refusal without a reason teaches the model nothing, because it cannot
// tell a policy it should respect from a failure worth retrying.
func refusedToolResult(invocation ToolInvocation, refusal ContentBlock) ToolResult {
	reason := fmt.Sprintf("tool %q was refused by policy before it ran", invocation.ToolName)
	if text, unquoted := strconv.Unquote(string(refusal.Output)); unquoted == nil && text != "" {
		reason = text
	}
	result := errorToolResult(invocation, errors.New(reason))
	if len(refusal.Output) > 0 {
		result.Output = cloneRawJSON(refusal.Output)
	}
	return result
}

func (a *agent) executeTool(ctx context.Context, invocation ToolInvocation) (result ToolResult) {
	tool, exists := a.tools[invocation.ToolName]
	if !exists {
		return errorToolResult(invocation, fmt.Errorf("tool %q is not registered", invocation.ToolName))
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result = errorToolResult(invocation, fmt.Errorf("tool %q panicked: %v", invocation.ToolName, recovered))
		}
	}()
	toolResult, err := tool.Execute(ctx, cloneRawJSON(invocation.Input))
	if err != nil {
		return errorToolResult(invocation, err)
	}
	if len(toolResult.Output) == 0 {
		toolResult.Output = json.RawMessage(`null`)
	}
	if !json.Valid(toolResult.Output) {
		return errorToolResult(invocation, errors.New("tool returned invalid JSON output"))
	}
	toolResult.ToolCallID = invocation.ToolCallID
	toolResult.ToolName = invocation.ToolName
	return toolResult
}

func (a *agent) newUserMessage(input Input) (Message, error) {
	messageID, err := generateUUID()
	if err != nil {
		return Message{}, fmt.Errorf("core: generate user message ID: %w", err)
	}
	block := ContentBlock{Type: input.Type}
	switch input.Type {
	case InputTypeText:
		block.Text = input.Content
	case InputTypeImage:
		block.ImageData = input.Content
		block.ImageSource, _ = input.Metadata["image_source"].(string)
		block.MediaFormat, _ = input.Metadata["media_format"].(string)
	case InputTypeStructured:
		block.Text = input.Content
		block.Input = cloneRawJSON(input.Data)
	}
	return Message{
		ID:        messageID,
		Role:      RoleUser,
		Content:   []ContentBlock{block},
		CreatedAt: time.Now().UTC(),
		Metadata:  cloneMetadata(input.Metadata),
	}, nil
}

func (a *agent) emitPartial(
	ctx context.Context,
	events chan<- Event,
	messageID string,
	chunk StreamChunk,
) error {
	return a.emit(ctx, events, EventTypePartial, StreamPayload{MessageID: messageID, Chunk: chunk}, "")
}

func (a *agent) emitStreamCompletion(ctx context.Context, events chan<- Event, messageID string) error {
	return a.emit(ctx, events, EventTypeStream, StreamPayload{
		MessageID: messageID,
		Chunk:     StreamChunk{Type: StreamTypeDone},
	}, "")
}

func (a *agent) emitToolCalls(
	ctx context.Context,
	events chan<- Event,
	messageID string,
	calls []ContentBlock,
) error {
	return a.emit(ctx, events, EventTypeToolCall, ToolCallPayload{
		MessageID: messageID,
		Calls:     cloneContentBlocks(calls),
	}, "")
}

func (a *agent) emitToolResults(ctx context.Context, events chan<- Event, results []ToolResult) error {
	cloned := make([]ToolResult, len(results))
	for index, result := range results {
		cloned[index] = result
		cloned[index].Output = cloneRawJSON(result.Output)
	}
	return a.emit(ctx, events, EventTypeToolResult, ToolResultPayload{Results: cloned}, "")
}

func (a *agent) emitStateChange(
	ctx context.Context,
	events chan<- Event,
	message Message,
) error {
	return a.emit(ctx, events, EventTypeStateChange, StateChangePayload{
		Reason:  stateChangeReason(message),
		Message: cloneMessage(message),
	}, "")
}

func (a *agent) emitError(ctx context.Context, events chan<- Event, operation string, err error) error {
	return a.emit(ctx, events, EventTypeError, ErrorPayload{
		Operation: operation,
		Message:   err.Error(),
	}, err.Error())
}

func (a *agent) emit(
	ctx context.Context,
	events chan<- Event,
	eventType EventType,
	data any,
	errorText string,
) error {
	event, err := newEvent(eventType, data)
	if err != nil {
		return err
	}
	event.Error = errorText
	a.setEventIdentity(&event)
	a.bus.Publish(ctx, event)
	select {
	case events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *agent) setEventIdentity(event *Event) {
	event.SessionID = a.config.SessionID
	event.AgentID = a.config.AgentID
}

type messageAccumulator struct {
	content    []ContentBlock
	responseID string
}

func (a *messageAccumulator) add(chunk StreamChunk) error {
	if chunk.Err != nil {
		return chunk.Err
	}
	if chunk.ResponseID != "" {
		a.responseID = chunk.ResponseID
	}
	if chunk.EncryptedContent != "" {
		a.appendOrMerge(ContentBlock{Type: ContentTypeReasoning, EncryptedContent: chunk.EncryptedContent})
	}
	switch chunk.Type {
	case StreamTypeText:
		a.appendOrMerge(ContentBlock{Type: ContentTypeText, Text: chunk.Text})
	case StreamTypeThinking:
		a.appendOrMerge(ContentBlock{
			Type:        ContentTypeThinking,
			Thinking:    chunk.Text,
			ThinkingSig: chunk.ThinkingSig,
		})
	case StreamTypeToolCall:
		if chunk.ToolCall == nil {
			return errors.New("core: tool_call chunk has no tool call")
		}
		call := cloneContentBlocks([]ContentBlock{*chunk.ToolCall})[0]
		if len(call.Input) == 0 {
			call.Input = json.RawMessage(`{}`)
		}
		if err := validateContentBlock(call); err != nil {
			return fmt.Errorf("core: invalid tool call: %w", err)
		}
		a.content = append(a.content, call)
	case StreamTypeDone:
		return nil
	case StreamTypeError:
		if chunk.Text == "" {
			return errors.New("core: provider stream returned an unspecified error")
		}
		return errors.New(chunk.Text)
	default:
		return fmt.Errorf("core: unsupported stream chunk type %q", chunk.Type)
	}
	return validateToolCalls(a.toolCalls())
}

func (a *messageAccumulator) appendOrMerge(block ContentBlock) {
	if len(a.content) > 0 {
		last := &a.content[len(a.content)-1]
		if last.Type == block.Type {
			switch block.Type {
			case ContentTypeText:
				last.Text += block.Text
			case ContentTypeThinking:
				last.Thinking += block.Thinking
				if block.ThinkingSig != "" {
					last.ThinkingSig = block.ThinkingSig
				}
			case ContentTypeReasoning:
				last.EncryptedContent = block.EncryptedContent
			}
			return
		}
	}
	a.content = append(a.content, block)
}

func (a *messageAccumulator) toolCalls() []ContentBlock {
	if a == nil {
		return nil
	}
	calls := make([]ContentBlock, 0)
	for _, block := range a.content {
		if block.Type == ContentTypeToolUse {
			calls = append(calls, cloneContentBlocks([]ContentBlock{block})[0])
		}
	}
	return calls
}

func (a *messageAccumulator) message(messageID string, incomplete bool) (Message, bool) {
	if a == nil || (len(a.content) == 0 && a.responseID == "") {
		return Message{}, false
	}
	var metadata map[string]any
	if a.responseID != "" {
		metadata = map[string]any{"response_id": a.responseID}
	}
	return Message{
		ID:         messageID,
		Role:       RoleAssistant,
		Content:    cloneContentBlocks(a.content),
		CreatedAt:  time.Now().UTC(),
		Metadata:   metadata,
		Incomplete: incomplete,
	}, true
}

func validateToolCalls(calls []ContentBlock) error {
	seen := make(map[string]struct{}, len(calls))
	for index, call := range calls {
		if call.Type != ContentTypeToolUse {
			return fmt.Errorf("core: tool call %d has type %q", index, call.Type)
		}
		if err := validateContentBlock(call); err != nil {
			return fmt.Errorf("core: tool call %d: %w", index, err)
		}
		if _, exists := seen[call.ToolCallID]; exists {
			return fmt.Errorf("core: duplicate tool call ID %q", call.ToolCallID)
		}
		seen[call.ToolCallID] = struct{}{}
	}
	return nil
}

func validateToolInvocation(invocation ToolInvocation) error {
	return validateContentBlock(ContentBlock{
		Type:       ContentTypeToolUse,
		ToolCallID: invocation.ToolCallID,
		ToolName:   invocation.ToolName,
		Input:      invocation.Input,
	})
}

func validateToolResult(result ToolResult) error {
	if result.ToolCallID == "" || result.ToolName == "" {
		return errors.New("core: tool result ID and name must not be empty")
	}
	if len(result.Output) == 0 {
		return errors.New("core: tool result output must not be empty")
	}
	// The output has to be valid JSON because it is carried as a json.RawMessage
	// into the event stream, the transcript and the journal, and a value that
	// cannot be marshalled is a record that cannot be written down. A hook that
	// wants to wrap a result in an untrusted envelope therefore has to produce
	// JSON, which the security part does for this reason.
	if !json.Valid(result.Output) {
		return errors.New("core: tool result output must be valid JSON")
	}
	return nil
}

func errorToolResult(invocation ToolInvocation, err error) ToolResult {
	output := append(json.RawMessage(`{"error":`), strconv.AppendQuote(nil, err.Error())...)
	output = append(output, '}')
	return ToolResult{
		ToolCallID: invocation.ToolCallID,
		ToolName:   invocation.ToolName,
		Output:     output,
		IsError:    true,
		Error:      err.Error(),
	}
}

func toolResultMessages(results []ToolResult) ([]Message, error) {
	messages := make([]Message, len(results))
	for index, result := range results {
		messageID, err := generateUUID()
		if err != nil {
			return nil, fmt.Errorf("core: generate tool result message ID: %w", err)
		}
		messages[index] = Message{
			ID:        messageID,
			Role:      RoleTool,
			Content:   []ContentBlock{toolResultBlock(result)},
			CreatedAt: time.Now().UTC(),
		}
	}
	return messages, nil
}

func toolResultBlock(result ToolResult) ContentBlock {
	return ContentBlock{
		Type:       ContentTypeToolResult,
		ToolCallID: result.ToolCallID,
		ToolName:   result.ToolName,
		Output:     cloneRawJSON(result.Output),
		IsError:    result.IsError,
	}
}

func cloneToolSpecs(specs []ToolSpec) []ToolSpec {
	if specs == nil {
		return nil
	}
	cloned := make([]ToolSpec, len(specs))
	for index, spec := range specs {
		cloned[index] = spec
		cloned[index].Parameters = cloneRawJSON(spec.Parameters)
	}
	return cloned
}

var generateUUID = newUUID
var readRandomBytes = rand.Read

func newUUID() (string, error) {
	var value [16]byte
	if _, err := readRandomBytes(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
