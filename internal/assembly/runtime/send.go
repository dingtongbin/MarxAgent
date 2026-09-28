// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/assembly/mode"
	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// maxMessageBytes bounds one message from the interface.
//
// A message is a person typing, and a request that carries four megabytes of
// text is not a person typing. The bound is generous enough that nothing
// legitimate is refused and small enough that the journal write behind it cannot
// be made to allocate without limit by a request.
const maxMessageBytes = 512 << 10

// Send appends a message to a conversation and runs the turn it starts.
//
// It returns as soon as the turn is under way. The turn itself is what the
// interface watches on the event stream, so making the request wait for it would
// mean a browser tab holding a request open for as long as a model takes to
// answer, which is a request that a proxy anywhere along the way will cut off.
//
// An error from here is an error about the request: no model, already running,
// nothing configured. Anything that goes wrong during the turn arrives on the
// stream as an error event, because by then the answer is already owed to the
// window.
func (r *Runtime) Send(ctx context.Context, sessionID string, request SendRequest) (SessionInfo, error) {
	session, err := r.lookup(sessionID)
	if err != nil {
		return SessionInfo{}, err
	}
	// The caller's context is checked but not kept. A turn outlives the request
	// that started it: that request ends when the message is accepted, and a turn
	// bound to it would be cancelled by its own answer arriving.
	if err := ctx.Err(); err != nil {
		return SessionInfo{}, err
	}
	text := strings.TrimSpace(request.Text)
	if text == "" {
		return SessionInfo{}, ErrEmptyMessage
	}
	if len(text) > maxMessageBytes {
		return SessionInfo{}, fmt.Errorf("runtime: a message must be at most %d bytes", maxMessageBytes)
	}
	// The turn runs at the level it was sent under. Naming one this build does
	// not have is refused here rather than run as something else, because a
	// turn that ran at a level nobody asked for is a turn whose reach nobody
	// can account for.
	exec, err := mode.NormalizeExecution(request.Execution)
	if err != nil {
		return SessionInfo{}, err
	}

	reference, err := r.options.Config.Resolve(request.Model)
	if err != nil {
		if session.info.Model == "" {
			return SessionInfo{}, fmt.Errorf("%w: %v", ErrNoModel, err)
		}
		// A conversation that already has a model keeps it when the request names
		// one that cannot be resolved, because a switch that silently failed would
		// leave a person reading an answer from a model they had stopped using.
		reference, err = r.options.Config.Resolve(session.info.Model)
		if err != nil {
			return SessionInfo{}, fmt.Errorf("%w: %v", ErrNoModel, err)
		}
	}
	provider, err := r.options.Gateway(reference)
	if err != nil {
		return SessionInfo{}, fmt.Errorf("%w: %v", ErrNoModel, err)
	}

	r.mu.Lock()
	if session.running {
		r.mu.Unlock()
		return SessionInfo{}, fmt.Errorf("%w: %s", ErrBusy, sessionID)
	}
	changed := false
	if session.buffer == nil {
		buffer, err := r.bufferFor(session)
		if err != nil {
			r.mu.Unlock()
			return SessionInfo{}, err
		}
		session.buffer = buffer
	}
	history := append([]core.Message(nil), session.messages...)
	// The conversation remembers which model it is on as a specification, because that
	// is what names a provider as well as a model and the specification is what a later
	// turn has to resolve again. The model itself is what the provider knows the model
	// by, and it is that name rather than our specification of it that goes into the
	// request: a provider told `deepseek:deepseek-flash` answers with a 400 naming the
	// models it does support, and it is right to.
	spec := reference.Spec()
	session.info.Model = spec
	// A coding conversation remembers the level its next turn was sent under,
	// which is both the level this turn runs at and what the window shows
	// until the next one says otherwise.
	if session.info.Mode == ModeCoding && string(exec) != session.info.Execution {
		session.info.Execution = string(exec)
		changed = true
	}
	session.generation++
	generation := session.generation
	runCtx, cancel := context.WithCancel(r.ctx)
	session.runCancel = cancel
	session.running = true
	info := r.infoLocked(session)
	r.mu.Unlock()

	if changed {
		r.persistSummary(session)
	}

	agent, err := r.assemble(ctx, provider, session, reference.Model, history, exec)
	if err != nil {
		cancel()
		r.mu.Lock()
		session.running = false
		session.runCancel = nil
		r.mu.Unlock()
		return SessionInfo{}, err
	}
	r.hub.publishState(info)

	go r.runTurn(runCtx, cancel, session, agent, text, generation)
	return info, nil
}

// assemble builds the agent for one turn.
//
// A new agent per turn rather than one per conversation, because the core's
// state is its conversation and the conversation continues: the new agent is
// handed the messages the last one produced as its initial state. That keeps the
// prefix cache honest, because the history the model sees is the same history
// whatever object happens to be holding it.
//
// The mode decides the assembly. A coding conversation gets the coding mode's
// tools, gates and standing context; a chat one gets the chat mode's empty tool
// set behind its gate, because words were what it was opened for.
func (r *Runtime) assemble(
	ctx context.Context,
	provider core.Provider,
	session *sessionState,
	model string,
	history []core.Message,
	exec mode.ExecutionMode,
) (core.Agent, error) {
	if session.info.Mode == ModeCoding {
		return r.assembleCoding(ctx, provider, session, model, history, exec)
	}
	return r.assembleChat(provider, session, model, history)
}

// assembleChat builds a chat turn.
//
// The chat assembly is stateless — no tools, no snapshot, no shell — so it is
// built per turn rather than kept, and the only thing it adds to the bare
// agent is its gate: a conversation without tools stays a conversation without
// tools, whatever a future turn's wiring says.
func (r *Runtime) assembleChat(
	provider core.Provider,
	session *sessionState,
	model string,
	history []core.Message,
) (core.Agent, error) {
	assembly := mode.NewChat()
	agent, err := core.NewAgent(provider, core.Config{
		AgentID:         "main-" + session.info.ID,
		SessionID:       session.info.ID,
		Model:           model,
		SystemHint:      assembly.SystemPrompt(),
		MaxIterations:   core.DefaultMaxIterations,
		InitialMessages: history,
		Metadata: map[string]any{
			"mode":    session.info.Mode,
			"project": session.info.Project,
		},
	})
	if err != nil {
		return nil, err
	}
	if err := assembly.RegisterHooks(agent); err != nil {
		return nil, err
	}
	return agent, nil
}

// assembleCoding builds a coding turn on the session's own assembly.
//
// The assembly carries the project's tools and gates, so it is built once per
// conversation and reused: it walks the project once, opens the shell once and
// hands the same bytes of standing context to every turn, which is what keeps
// the prompt part of the prefix cache honest. The execution mode picks which
// reach the turn runs at, and the reach's tools and gate are built on the
// assembly once as well.
func (r *Runtime) assembleCoding(
	ctx context.Context,
	provider core.Provider,
	session *sessionState,
	model string,
	history []core.Message,
	exec mode.ExecutionMode,
) (core.Agent, error) {
	assembly, err := r.codingAssembly(session)
	if err != nil {
		return nil, err
	}
	toolSet, err := assembly.Tools(ctx, exec)
	if err != nil {
		return nil, err
	}
	agent, err := core.NewAgent(provider, core.Config{
		AgentID:         "main-" + session.info.ID,
		SessionID:       session.info.ID,
		Model:           model,
		SystemHint:      assembly.SystemPrompt(),
		MaxIterations:   mode.CodingMaxIterations,
		InitialMessages: history,
		Tools:           toolSet,
		Metadata: map[string]any{
			"mode":      session.info.Mode,
			"project":   session.info.Project,
			"execution": string(exec),
		},
	})
	if err != nil {
		return nil, err
	}
	if err := assembly.RegisterHooks(agent, exec); err != nil {
		return nil, err
	}
	return agent, nil
}

// codingAssembly builds or returns the coding assembly of one conversation.
//
// The caller holds no lock while this runs, and a conversation that is running
// a turn cannot be sent another one, so at most one assembly is ever being
// built for a session at a time. The write back to the session is under the
// runtime's lock, which is the same lock every read of the session takes.
//
// The parts come from the installation's own places: the skills and the sub
// agent templates live beside the application's data and inside the project,
// the wiki lives in the project's data directory, the language server is the
// Go server while the project has one, and the broker writes through the
// session's own durability pipeline.
func (r *Runtime) codingAssembly(session *sessionState) (*mode.Coding, error) {
	r.mu.Lock()
	assembly := session.coding
	project := session.info.Project
	sessionID := session.info.ID
	buffer := session.buffer
	r.mu.Unlock()
	if assembly != nil {
		return assembly, nil
	}
	built, err := mode.NewCoding(project, mode.Parts{
		SkillGlobalDir:     skillsGlobalDir(),
		SkillProjectDir:    filepath.Join(project, storage.ProjectDirName, "skills"),
		TemplateGlobalDir:  templatesGlobalDir(),
		TemplateProjectDir: filepath.Join(project, storage.ProjectDirName, "agents"),
		LSPCommand:         languageServerCommand(project),
		WikiRoot:           filepath.Join(project, storage.ProjectDirName, "wiki"),
		AgentJournal:       brokerJournal{buffer: buffer},
		AgentMainID:        "main-" + sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime: assemble the coding mode: %w", err)
	}
	r.mu.Lock()
	if session.coding != nil {
		// Two calls to this function for the same session: the first one's
		// assembly is the one that stays, because its snapshot is already in
		// front of the history the conversation was built with.
		r.mu.Unlock()
		return session.coding, nil
	}
	session.coding = built
	r.mu.Unlock()
	return built, nil
}

// skillsGlobalDir is where the user's skills are installed, beside the
// application's own data.
func skillsGlobalDir() string {
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, storage.AppDirName, "skills")
}

// templatesGlobalDir is where the user's sub agent templates are installed.
func templatesGlobalDir() string {
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, storage.AppDirName, "agents")
}

// languageServerCommand names the server a project carries. A Go project
// opens gopls; a project with none is a conversation with no lsp tools, which
// is a normal session rather than a misconfigured one.
func languageServerCommand(project string) []string {
	if _, err := os.Stat(filepath.Join(project, "go.mod")); err != nil {
		return nil
	}
	return []string{"gopls"}
}

// brokerJournal is the durability sink the sub agent broker writes through:
// the session's own write buffer, which batches, fronted by the promise the
// broker makes that the bytes are down before anything reads them.
type brokerJournal struct {
	buffer *storage.WriteBuffer
}

func (j brokerJournal) WriteSync(ctx context.Context, records []storage.Record) error {
	if j.buffer == nil {
		return fmt.Errorf("runtime: the session has no journal to write through")
	}
	for _, record := range records {
		if err := j.buffer.Append(record); err != nil {
			return err
		}
	}
	return j.buffer.Flush(ctx)
}

func (j brokerJournal) LastSeq(_ context.Context, stream string) (int64, error) {
	if j.buffer == nil {
		return 0, fmt.Errorf("runtime: the session has no journal to read from")
	}
	return j.buffer.LastSeq(stream), nil
}

// runTurn drives one turn to its end.
//
// Every event the core emits goes two places: onto the stream every window
// watching this conversation is reading, and into the journal, which is what the
// conversation is rebuilt from after a restart. The journal write goes through
// the buffer rather than straight to the file, so a burst of streaming tokens
// does not become one system call each.
//
// A turn that failed says so on the stream, not in a return value: by the time
// the events have been read, the window is already owed an answer. So the error
// is taken off the stream as it goes past, and the state the conversation is left
// in is the answer to it. A turn that failed and a turn that ended look the same
// to a caller who is only waiting for the channel to close, and the difference
// between a conversation marked degraded and one marked checkpointed is the whole
// reason a window can say something useful about it.
func (r *Runtime) runTurn(
	ctx context.Context,
	cancel context.CancelFunc,
	session *sessionState,
	agent core.Agent,
	text string,
	generation int,
) {
	defer cancel()

	events, err := agent.Run(ctx, core.Input{Type: core.InputTypeText, Content: text})
	if err != nil {
		r.finishTurn(session, generation, fmt.Errorf("runtime: start the turn: %w", err))
		return
	}
	var runErr error
	for event := range events {
		if event.Type == core.EventTypeError && runErr == nil {
			runErr = errors.New(event.Error)
			if runErr.Error() == "" {
				runErr = errors.New("the turn failed")
			}
		}
		r.forwardEvent(session, event)
	}
	if ctx.Err() != nil && runErr == nil {
		// A turn that was interrupted is not a turn that failed. The messages
		// that arrived are kept and the last one is marked incomplete by the core,
		// which is a record of what was said rather than an error to show.
		runErr = ctx.Err()
	}
	r.finishTurn(session, generation, runErr)
}

// forwardEvent records one event and shows it to whoever is watching.
func (r *Runtime) forwardEvent(session *sessionState, event core.Event) {
	// A message is written down the moment it is appended to the state, because
	// that is the moment it exists as far as the conversation is concerned. The
	// event carries the whole message, so the journal and the interface are given
	// the same value rather than two renderings of it.
	if payload, ok := decodeStateChange(event); ok {
		r.appendMessage(session, payload)
	}
	r.hub.publish(session.info.ID, event)
}

// appendMessage records an appended message, writes it to the journal, and gives
// the conversation its title the first time anything is said in it.
func (r *Runtime) appendMessage(session *sessionState, message core.Message) {
	r.mu.Lock()
	// The core appends a message and publishes the change; the runtime keeps its
	// own copy so a window opened after the turn can read the history. A message
	// already recorded is not recorded twice, which matters because an
	// interrupted stream appends the same message again marked incomplete.
	if index := indexOfMessage(session.messages, message.ID); index >= 0 {
		session.messages[index] = message
	} else {
		session.messages = append(session.messages, message)
	}
	if strings.TrimSpace(session.info.Title) == "" {
		session.info.Title = titleOf(message)
	}
	session.info.UpdatedAt = r.options.Now().UTC()
	stream := storage.StreamSession + ":" + session.info.ID
	eventsStream := storage.StreamEvents + ":" + session.info.ID
	buffer := session.buffer
	info := r.infoLocked(session)
	r.mu.Unlock()

	encoded, err := json.Marshal(message)
	if err != nil {
		r.log("message did not encode", map[string]any{
			"session_id": session.info.ID,
			"message_id": message.ID,
			"error":      err.Error(),
		})
		return
	}
	if buffer == nil {
		return
	}
	// Two records, one per stream, and the sequence is assigned per stream, which
	// is what lets recovery tell a half-written conversation from an interrupted
	// one.
	if err := buffer.Append(storage.Record{
		Stream: stream, Type: storage.RecordMessage, Data: encoded,
	}); err != nil {
		r.log("message did not reach the journal", map[string]any{
			"session_id": session.info.ID, "error": err.Error(),
		})
	}
	if err := buffer.Append(storage.Record{
		Stream: eventsStream, Type: storage.RecordEvent, Data: eventBytes(message.ID, "state_change", encoded),
	}); err != nil {
		r.log("event did not reach the journal", map[string]any{
			"session_id": session.info.ID, "error": err.Error(),
		})
	}
	r.hub.publishState(info)
}

// indexableMessage turns a message record into the shape the ledger indexes.
//
// The journal keeps the message as it was, because that is the copy a
// conversation is rebuilt from. The ledger keeps it as text, because that is the
// copy a full text search can take: a content block list is not something an
// index can be given, and indexing the raw JSON would make a search for a word
// somebody said find the word `content` instead.
//
// Every kind of block contributes. A message that is only a tool result is as
// much a record of what happened as one that is only words, and a ledger that
// could not find it would be a ledger that answered questions about the
// conversation while saying nothing about what the assistant did.
func indexableMessage(record storage.Record) (storage.Record, error) {
	if record.Type != storage.RecordMessage {
		return record, nil
	}
	var message core.Message
	if err := json.Unmarshal(record.Data, &message); err != nil {
		return storage.Record{}, fmt.Errorf("runtime: decode a message for the ledger: %w", err)
	}
	var text strings.Builder
	for _, block := range message.Content {
		switch block.Type {
		case core.ContentTypeText:
			text.WriteString(block.Text)
		case core.ContentTypeThinking:
			text.WriteString(block.Thinking)
		case core.ContentTypeToolUse:
			text.WriteString(block.ToolName)
			text.WriteString(" ")
			text.Write(block.Input)
		case core.ContentTypeToolResult:
			text.Write(block.Output)
		}
		text.WriteString("\n")
	}
	encoded, err := json.Marshal(struct {
		ID         string `json:"id"`
		Role       string `json:"role"`
		Content    string `json:"content"`
		Incomplete bool   `json:"incomplete,omitempty"`
	}{
		ID:         message.ID,
		Role:       string(message.Role),
		Content:    text.String(),
		Incomplete: message.Incomplete,
	})
	if err != nil {
		return storage.Record{}, fmt.Errorf("runtime: encode a message for the ledger: %w", err)
	}
	record.Data = encoded
	return record, nil
}

// eventBytes builds a journal record body for an event.
//
// The body carries the same message the message record does rather than the
// core's own event envelope, because the ledger's event table is read by people
// querying it and an envelope with a JSON string inside it is something a person
// has to unwrap before they can read it.
func eventBytes(messageID, kind string, data json.RawMessage) json.RawMessage {
	payload := struct {
		MessageID string          `json:"message_id,omitempty"`
		Kind      string          `json:"kind"`
		Message   json.RawMessage `json:"message,omitempty"`
	}{MessageID: messageID, Kind: kind, Message: data}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return encoded
}

func indexOfMessage(messages []core.Message, id string) int {
	for index := range messages {
		if messages[index].ID == id {
			return index
		}
	}
	return -1
}

// titleOf builds a conversation's title from its first message.
func titleOf(message core.Message) string {
	var text string
	for _, block := range message.Content {
		if block.Type == core.ContentTypeText && strings.TrimSpace(block.Text) != "" {
			text = strings.TrimSpace(block.Text)
			break
		}
	}
	// One line, and not a very long one: this is what a person scans in a 232
	// pixel column.
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, "\n", " ")), " ")
	runes := []rune(text)
	if len(runes) > 40 {
		return string(runes[:40]) + "…"
	}
	return text
}

// finishTurn settles a turn: the state the conversation is left in, the
// checkpoint, and the boundary flush.
func (r *Runtime) finishTurn(session *sessionState, generation int, runErr error) {
	r.mu.Lock()
	if generation < session.generation {
		// A turn that was already superseded. Its events were shown while it ran,
		// but it does not get to say what state the conversation is left in.
		r.mu.Unlock()
		return
	}
	// The conversation is left in the state it is still in, on purpose, for the length
	// of the flush below. It is still running in the only sense that matters — it is
	// still being written down — and saying it had finished while the journal did not
	// yet hold the turn would be a state a window could read and act on, and read
	// wrong. The turn ends when its last record is on disk.
	info := r.infoLocked(session)
	buffer := session.buffer
	r.mu.Unlock()

	if err := r.persistSummary(session); err != nil {
		r.log("session summary did not save", map[string]any{
			"session_id": session.info.ID, "error": err.Error(),
		})
	}
	// The boundary flush: a turn that has ended is a moment where the journal is
	// made durable, rather than waiting for the periodic trigger to notice.
	if buffer != nil {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), flushBudget)
		if err := buffer.Flush(flushCtx); err != nil {
			r.log("session did not flush", map[string]any{
				"session_id": session.info.ID, "error": err.Error(),
			})
		}
		cancel()
	}

	r.mu.Lock()
	session.running = false
	session.runCancel = nil
	session.info.UpdatedAt = r.options.Now().UTC()
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		session.info.State = StateDegraded
	} else {
		session.info.State = StateCheckpointed
	}
	info = r.infoLocked(session)
	r.mu.Unlock()
	r.hub.publishState(info)
}

// flushBudget bounds one boundary flush.
//
// It is the same bound the shutdown uses, because the two are the same promise:
// a conversation that has ended is on disk before anything else is reported
// about it.
const flushBudget = 5 * time.Second

// Interrupt stops a turn that is running.
//
// A turn stopped this way ends the same way a turn that ran out of iterations
// does: the messages that arrived are kept, the one in flight is marked
// incomplete by the core, and the conversation is left where it got to. It is not
// rolled back, because a half-written answer is a record of what was said and
// discarding it would be discarding the record.
func (r *Runtime) Interrupt(sessionID string) error {
	session, err := r.lookup(sessionID)
	if err != nil {
		return err
	}
	r.mu.Lock()
	running := session.running
	cancel := session.runCancel
	r.mu.Unlock()
	if !running || cancel == nil {
		// Nothing is running, so there is nothing to stop. Reporting an error
		// would make a window that pressed the stop twice look broken.
		return nil
	}
	cancel()
	return nil
}

// persistSummary writes the session's metadata next to its journals.
func (r *Runtime) persistSummary(session *sessionState) error {
	r.mu.Lock()
	info := session.info
	messages := len(session.messages)
	scope := session.scope
	buffer := session.buffer
	r.mu.Unlock()
	if scope == nil {
		return nil
	}
	summary := storage.SessionSummary{
		SessionID:    info.ID,
		Mode:         info.Mode,
		CreatedAt:    info.CreatedAt,
		UpdatedAt:    info.UpdatedAt,
		MessageCount: messages,
		Summary:      info.Title,
		Project:      info.Project,
		Execution:    info.Execution,
		Model:        info.Model,
	}
	if buffer != nil {
		summary.LastSeq = buffer.LastSeq(storage.StreamSession + ":" + info.ID)
	}
	if err := scope.WriteSessionSummary(summary); err != nil {
		return fmt.Errorf("runtime: write session summary: %w", err)
	}
	// The checkpoint is the ledger's own pointer into the journal, which is what
	// makes a replay after a crash idempotent rather than duplicated.
	return scope.CheckpointStream(context.Background(), storage.StreamSession+":"+info.ID, summary.LastSeq)
}

// decodeStateChange reads the message out of a state change event.
func decodeStateChange(event core.Event) (core.Message, bool) {
	if event.Type != core.EventTypeStateChange || event.Data == "" {
		return core.Message{}, false
	}
	var payload core.StateChangePayload
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		return core.Message{}, false
	}
	if payload.Message.ID == "" {
		return core.Message{}, false
	}
	return payload.Message, true
}
