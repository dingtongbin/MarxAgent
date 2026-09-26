// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// OrchestrationConfig builds the tools the main core uses to direct sub agents.
//
// The tools are built here rather than in the assembly layer because they are pure
// translations: a tool call becomes one call on the pool, and the answer becomes
// JSON. Which pool, and whether the main core has these at all, is the assembly
// layer's decision and is why this is a constructor and not a package global.
type OrchestrationConfig struct {
	// Pool is the pool to direct. It is required.
	Pool *Pool
	// MainID is who the tools act as. It must be the pool's main core, so that a
	// misconfigured tool cannot acquire powers the pool would refuse it.
	MainID string
	// WaitFor decides how long read_result will wait before giving up. Zero means do
	// not wait, which is the right default for a model that has other things to do.
	WaitFor time.Duration
}

// Orchestrator is the set of tools the main core uses to create, direct and read its
// sub agents.
//
// Every one of them refuses a caller that is not the main core. That is enforced
// twice over, on purpose: the tools are only ever handed to the main core, and the
// pool refuses a non-main caller anyway. Either alone would do, but either alone is
// one mistake away from a sub agent that can spawn more of itself, so the rule does
// not rest on a single thing holding.
type Orchestrator struct {
	pool    *Pool
	mainID  string
	waitFor time.Duration
}

// NewOrchestrator builds the orchestration tools.
func NewOrchestrator(config OrchestrationConfig) (*Orchestrator, error) {
	if config.Pool == nil {
		return nil, fmt.Errorf("subagent: the orchestration tools need a pool")
	}
	if config.MainID == "" {
		return nil, fmt.Errorf("subagent: the orchestration tools need a main core to act as")
	}
	if config.MainID != config.Pool.MainID() {
		return nil, fmt.Errorf(
			"subagent: the orchestration tools would act as %q but the pool's main core is %q",
			config.MainID, config.Pool.MainID())
	}
	return &Orchestrator{
		pool:    config.Pool,
		mainID:  config.MainID,
		waitFor: config.WaitFor,
	}, nil
}

// Tools returns the tools, in a stable order so a request declaring them is byte for
// byte the same between turns and the prefix cache keeps hitting.
func (o *Orchestrator) Tools() []core.Tool {
	return []core.Tool{
		&spawnTool{o: o},
		&startTool{o: o},
		&stopTool{o: o},
		&deleteTool{o: o},
		&listTool{o: o},
		&readResultTool{o: o},
		&sendMessageTool{o: o},
		&readMessageTool{o: o},
	}
}

// jsonResult builds a successful result from anything marshalable.
func jsonResult(value any) (core.ToolResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("subagent: encode the tool answer: %w", err)
	}
	return core.ToolResult{Output: encoded}, nil
}

// refusal builds the result of a tool that ran and said no.
//
// It is a result rather than an error because a model that asked for something it may
// not have needs to be told so in a form it will read and act on. Returning an error
// would have it retried or abandoned rather than understood.
func refusal(reason string) core.ToolResult {
	encoded, err := json.Marshal(map[string]string{"refused": reason})
	if err != nil {
		// A map of one string always marshals, so this cannot happen; reporting a
		// plain refusal is still better than a result with no body.
		encoded = []byte(`{"refused":"the request was refused"}`)
	}
	return core.ToolResult{Output: encoded, IsError: true, Error: reason}
}

// decodeArgs reads a tool's arguments, refusing rather than guessing when they are
// wrong. A model that got the shape of a call wrong should be told the shape, not
// have its mistake quietly interpreted.
func decodeArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("subagent: the arguments do not fit: %w", err)
	}
	return nil
}

// schemaOf renders a schema that has already been written out by hand, copied so a
// caller cannot change it out from under the tool.
func schemaOf(schema string) json.RawMessage {
	return json.RawMessage(schema)
}

// listTemplates reports the templates a caller could start, so a model asked to
// delegate can find out what kinds of sub agent there are rather than guessing a
// name. The names and the one line descriptions are enough to choose; the
// instructions themselves are not, because they belong to the sub agent.
func (o *Orchestrator) listTemplates(ctx context.Context) []TemplateSummary {
	if o.pool.config.Templates == nil {
		return nil
	}
	// The index rather than a search, because the question is what exists rather
	// than what matches, and a search over everything would be a way of asking the
	// same thing while looking like it was doing something narrower.
	entries := o.pool.config.Templates.Index()
	if err := ctx.Err(); err != nil {
		return nil
	}
	summaries := make([]TemplateSummary, 0, len(entries))
	for _, entry := range entries {
		summaries = append(summaries, TemplateSummary{
			Name:        entry.Name,
			Description: entry.Description,
			Tools:       append([]string(nil), entry.Tools...),
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries
}

// TemplateSummary is a template as a caller choosing one is allowed to see it: the
// name, what it is for, and what it may reach. The instructions are not here, because
// they are the sub agent's and a parent that reads them has stopped delegating.
type TemplateSummary struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

// textInput builds the input for a task given as plain text.
func textInput(content string) core.Input {
	return core.Input{Type: core.InputTypeText, Content: content}
}

// firstLine takes the opening line of a task as its account.
//
// A task is often several paragraphs, and a summary is meant to be glanceable, so the
// first line is used rather than the whole thing. It is trimmed and shortened, because
// a summary that is itself a wall of text defeats the purpose of having one.
func firstLine(text string) string {
	line := text
	if index := strings.IndexAny(line, "\r\n"); index >= 0 {
		line = line[:index]
	}
	line = strings.TrimSpace(line)
	const limit = 120
	if len(line) > limit {
		// Cut on a rune boundary so a multi-byte character is never cut in half,
		// which would leave the summary holding bytes that are not a character.
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = strings.TrimSpace(line[:cut]) + "..."
	}
	if line == "" {
		return "an unnamed task"
	}
	return line
}

// Specs describes the tools the way a request declares them, so a main core can be
// given the orchestration tools without a translation step that would have to be kept
// in step with the tools themselves.
func (o *Orchestrator) Specs() []core.ToolSpec {
	tools := o.Tools()
	specs := make([]core.ToolSpec, 0, len(tools))
	for _, tool := range tools {
		specs = append(specs, core.ToolSpec{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  tool.Parameters(),
		})
	}
	return specs
}

// Names lists the tool names, which is what a caller enabling them one at a time
// needs.
func (o *Orchestrator) Names() []string {
	tools := o.Tools()
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}

// spawnTool creates a slot for a template. It is separate from starting because a
// main core often wants to place a sub agent and give it work in a later turn, and a
// tool that did both would force the choice.
type spawnTool struct{ o *Orchestrator }

func (t *spawnTool) Name() string { return "spawn_subagent" }

func (t *spawnTool) Description() string {
	return "Create a sub agent from a named template and return its id. The sub agent " +
		"does nothing until it is started. Call list_templates to see the names."
}

func (t *spawnTool) Parameters() json.RawMessage {
	return schemaOf(`{
  "type": "object",
  "properties": {
    "template": {
      "type": "string",
      "description": "The template to build the sub agent from."
    }
  },
  "required": ["template"]
}`)
}

func (t *spawnTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		Template string `json:"template"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return refusal(err.Error()), nil
	}
	slot, err := t.o.pool.CreateSlot(ctx, t.o.mainID, args.Template)
	if err != nil {
		return refusal(err.Error()), nil
	}
	return jsonResult(struct {
		ID        string            `json:"id"`
		Template  string            `json:"template"`
		State     SlotState         `json:"state"`
		Templates []TemplateSummary `json:"available_templates,omitempty"`
	}{slot.ID, slot.Template, slot.State, t.o.listTemplates(ctx)})
}

// startTool puts a task on a slot. The task runs on its own, so this returns as soon
// as the sub agent is under way; the answer arrives as a message or is read later.
type startTool struct{ o *Orchestrator }

func (t *startTool) Name() string { return "start_subagent" }

func (t *startTool) Description() string {
	return "Give a sub agent a task and let it run. Returns immediately with the sub " +
		"agent's id; it does not wait for the work to finish. Omit the id to let any " +
		"free sub agent take it. Collect the answer with read_result."
}

func (t *startTool) Parameters() json.RawMessage {
	return schemaOf(`{
  "type": "object",
  "properties": {
    "id": {
      "type": "string",
      "description": "Which sub agent to run. Omit to use any free one."
    },
    "task": {
      "type": "string",
      "description": "What the sub agent is to do, in full. It cannot ask you a question."
    },
    "summary": {
      "type": "string",
      "description": "A one line account of the task, kept as the sub agent's summary."
    }
  },
  "required": ["task"]
}`)
}

func (t *startTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		ID      string `json:"id"`
		Task    string `json:"task"`
		Summary string `json:"summary"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return refusal(err.Error()), nil
	}
	if args.Task == "" {
		return refusal("a sub agent needs a task"), nil
	}
	if args.Summary == "" {
		// A sub agent with no account of what it was asked is one nobody can read
		// about later, so a summary is made rather than left empty. The task is the
		// best thing available and the caller gave it.
		args.Summary = firstLine(args.Task)
	}
	slot, err := t.o.pool.Start(ctx, t.o.mainID, Task{
		Slot:    args.ID,
		Input:   textInput(args.Task),
		Summary: args.Summary,
	})
	if err != nil {
		return refusal(err.Error()), nil
	}
	return jsonResult(struct {
		ID    string    `json:"id"`
		State SlotState `json:"state"`
	}{slot.ID, slot.State})
}

// stopTool ends the work on a slot and leaves the slot alone. It is not delete,
// because a caller that stops a sub agent usually wants to use it again.
type stopTool struct{ o *Orchestrator }

func (t *stopTool) Name() string { return "stop_subagent" }

func (t *stopTool) Description() string {
	return "Stop the work a sub agent is doing, keeping the sub agent. Use " +
		"delete_subagent to remove it instead."
}

func (t *stopTool) Parameters() json.RawMessage {
	return schemaOf(`{
  "type": "object",
  "properties": {
    "id": {
      "type": "string",
      "description": "Which sub agent to stop."
    }
  },
  "required": ["id"]
}`)
}

func (t *stopTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return refusal(err.Error()), nil
	}
	if err := t.o.pool.StopSlot(ctx, t.o.mainID, args.ID); err != nil {
		return refusal(err.Error()), nil
	}
	record, _ := t.o.pool.GetSlot(args.ID)
	return jsonResult(struct {
		ID    string    `json:"id"`
		State SlotState `json:"state"`
	}{args.ID, record.State})
}

// deleteTool removes a slot for good. A busy slot is refused, because deleting one
// out from under a running task loses whatever it was doing.
type deleteTool struct{ o *Orchestrator }

func (t *deleteTool) Name() string { return "delete_subagent" }

func (t *deleteTool) Description() string {
	return "Remove a sub agent. It must not be busy; stop it first."
}

func (t *deleteTool) Parameters() json.RawMessage {
	return schemaOf(`{
  "type": "object",
  "properties": {
    "id": {
      "type": "string",
      "description": "Which sub agent to remove."
    }
  },
  "required": ["id"]
}`)
}

func (t *deleteTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return refusal(err.Error()), nil
	}
	if err := t.o.pool.DeleteSlot(ctx, t.o.mainID, args.ID); err != nil {
		return refusal(err.Error()), nil
	}
	return jsonResult(struct {
		ID      string `json:"id"`
		Deleted bool   `json:"deleted"`
	}{args.ID, true})
}

// listTool shows the whole picture. It is a read and takes nothing out of anything,
// so asking twice costs nothing and loses nothing.
type listTool struct{ o *Orchestrator }

func (t *listTool) Name() string { return "list_subagents" }

func (t *listTool) Description() string {
	return "List every sub agent with its state, what it is confined to, what it last " +
		"did, and how many messages it has not read."
}

func (t *listTool) Parameters() json.RawMessage {
	return schemaOf(`{
  "type": "object",
  "properties": {}
}`)
}

func (t *listTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	view := t.o.pool.View()
	return jsonResult(struct {
		MainID    string            `json:"main_id"`
		Capacity  int               `json:"capacity"`
		SubAgents []AgentView       `json:"sub_agents"`
		Counts    map[SlotState]int `json:"counts"`
		Templates []TemplateSummary `json:"available_templates,omitempty"`
	}{view.MainID, view.Capacity, view.SubAgents, view.Summary(), t.o.listTemplates(ctx)})
}

// readResultTool collects what a sub agent produced. It waits only as long as it was
// built to, because a model with other work to do should not be parked here.
type readResultTool struct{ o *Orchestrator }

func (t *readResultTool) Name() string { return "read_subagent_result" }

func (t *readResultTool) Description() string {
	return "Read what a sub agent produced. Waits briefly for work still running, and " +
		"says so plainly if the answer is not ready rather than inventing one."
}

func (t *readResultTool) Parameters() json.RawMessage {
	return schemaOf(`{
  "type": "object",
  "properties": {
    "id": {
      "type": "string",
      "description": "Which sub agent to read."
    }
  },
  "required": ["id"]
}`)
}

func (t *readResultTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return refusal(err.Error()), nil
	}
	waitCtx := ctx
	if t.o.waitFor > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, t.o.waitFor)
		defer cancel()
	}
	result, err := t.o.pool.Wait(waitCtx, args.ID)
	if err != nil {
		// A wait that ran out is not a failure of the sub agent, so what is said is
		// what the slot is doing now. A model told "still running" can decide, where
		// one told "failed" would go looking for a fault that is not there.
		if record, ok := t.o.pool.GetSlot(args.ID); ok && record.State == SlotBusy {
			return jsonResult(struct {
				ID      string    `json:"id"`
				Pending bool      `json:"pending"`
				State   SlotState `json:"state"`
				Runs    int64     `json:"runs"`
			}{args.ID, true, record.State, record.Runs})
		}
		return refusal(err.Error()), nil
	}
	return jsonResult(result)
}

// sendMessageTool hands a message to a sub agent, or to the main core from one. It is
// how work is directed at a sub agent that is already running, because a sub agent
// cannot ask a question and a task given up front cannot anticipate what it will find.
type sendMessageTool struct{ o *Orchestrator }

func (t *sendMessageTool) Name() string { return "send_subagent_message" }

func (t *sendMessageTool) Description() string {
	return "Send a message to a sub agent that is already running, to tell it what to " +
		"do next or to correct it. Delivered to its mailbox; it reads it when it can."
}

func (t *sendMessageTool) Parameters() json.RawMessage {
	return schemaOf(`{
  "type": "object",
  "properties": {
    "to": {
      "type": "string",
      "description": "Which sub agent to send to."
    },
    "content": {
      "type": "string",
      "description": "What to tell it."
    },
    "type": {
      "type": "string",
      "description": "task, result, query or response. Defaults to task."
    }
  },
  "required": ["to", "content"]
}`)
}

func (t *sendMessageTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		To      string `json:"to"`
		Content string `json:"content"`
		Type    string `json:"type"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return refusal(err.Error()), nil
	}
	if err := ctx.Err(); err != nil {
		return refusal("the turn that asked for this is over"), nil
	}
	if t.o.pool.config.Broker == nil {
		return refusal("the pool has no broker, so there are no messages"), nil
	}
	if args.Type == "" {
		args.Type = "task"
	}
	sent, err := t.o.pool.config.Broker.Send(ctx, Message{
		FromAgentID: t.o.mainID,
		ToAgentID:   args.To,
		Type:        args.Type,
		Content:     args.Content,
	})
	if err != nil {
		return refusal(err.Error()), nil
	}
	return jsonResult(struct {
		ID  string `json:"id"`
		To  string `json:"to"`
		Seq int64  `json:"seq"`
	}{sent.ID, sent.ToAgentID, sent.Sequence})
}

// readMessageTool collects messages waiting for a sub agent. It removes them, so
// asking again finds only what has arrived since.
type readMessageTool struct{ o *Orchestrator }

func (t *readMessageTool) Name() string { return "read_subagent_messages" }

func (t *readMessageTool) Description() string {
	return "Take the messages waiting for a sub agent. They are removed, so a second " +
		"call returns only what has arrived since. Use list_subagents to see how many " +
		"are waiting without taking them."
}

func (t *readMessageTool) Parameters() json.RawMessage {
	return schemaOf(`{
  "type": "object",
  "properties": {
    "id": {
      "type": "string",
      "description": "Which sub agent to read. Omit to read your own."
    },
    "limit": {
      "type": "integer",
      "description": "How many to take. Omit for all that are waiting."
    }
  }
}`)
}

func (t *readMessageTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		ID    string `json:"id"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return refusal(err.Error()), nil
	}
	target := args.ID
	if target == "" {
		// A sub agent reading its own mail is the ordinary case, and the main core
		// is the only one with a mailbox of its own to fall back on.
		target = t.o.mainID
	}
	taken, err := t.o.pool.Take(ctx, t.o.mainID, target, args.Limit)
	if err != nil {
		return refusal(err.Error()), nil
	}
	return jsonResult(struct {
		From     string    `json:"from"`
		Count    int       `json:"count"`
		Messages []Message `json:"messages"`
	}{target, len(taken), taken})
}
