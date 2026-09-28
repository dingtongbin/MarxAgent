// SPDX-License-Identifier: Apache-2.0

// Package runtime is the session runtime: it owns the conversations, runs the
// agent behind each of them, keeps what happened on disk, and hands the same
// event stream to every window watching a session.
//
// It is assembly rather than a part. The decisions it makes are the ones a part
// is not allowed to: that a conversation with a directory under it is a work
// session stored beside the code, one without is a chat session stored in the
// application data directory, and that a conversation is assembled when the
// first message arrives rather than when a tab is opened.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/assembly/config"
	"github.com/dingtongbin/MarxAgent/internal/assembly/mode"
	"github.com/dingtongbin/MarxAgent/internal/assembly/ui/web"
	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/gateway"
	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// Errors this package reports.
var (
	// ErrNoSession is a request for a conversation that is not there. A window
	// asking for a session that was closed, or that this process has never seen
	// since it restarted, gets this rather than a newly created empty one.
	ErrNoSession = errors.New("runtime: no such session")
	// ErrNoModel is a request to run without a model, which happens when nothing
	// is configured yet. It is its own error because the answer is a settings
	// page rather than a retry.
	ErrNoModel = errors.New("runtime: no model is configured")
	// ErrBusy is a turn sent to a conversation that is already running one.
	ErrBusy = errors.New("runtime: the session is already running a turn")
	// ErrClosed is use of a runtime that has been shut down.
	ErrClosed = errors.New("runtime: the runtime is closed")
	// ErrEmptyMessage is a request that is not a message. It is its own error
	// because the answer to it is "that field was empty", which is a different
	// thing from "the request could not be carried out".
	ErrEmptyMessage = errors.New("runtime: a message must not be empty")
)

// State is what a session is doing.
//
// The four values are the ones the ledger records, so the interface never has to
// invent a fifth: a session is either not running, running, checkpointed after a
// turn, or degraded because the last turn failed.
type State string

const (
	// StateIdle is a session with no turn in progress.
	StateIdle State = "idle"
	// StateRunning is a session with a turn in progress.
	StateRunning State = "running"
	// StateCheckpointed is a session whose last turn ended and was written down.
	StateCheckpointed State = "checkpointed"
	// StateDegraded is a session whose last turn failed. It is not deleted and not
	// hidden: the messages that did arrive are still there, and a person can see
	// that the turn stopped rather than guessing why it ended.
	StateDegraded State = "degraded"
)

// Conversation modes.
const (
	// ModeCoding is a conversation about a project directory: it assembles the
	// coding mode's tools, gates and standing context.
	ModeCoding = "coding"
	// ModeChat is a conversation with no directory, which runs on words alone.
	ModeChat = "chat"
	// ModeWork is what earlier builds stored a coding conversation as. It is
	// kept so a conversation written by one build reads as itself after an
	// upgrade, and it is never written again.
	ModeWork = "work"
)

// normalizeMode maps the modes a journal can carry onto the modes this build
// reports, so a conversation keeps its identity across an upgrade.
func normalizeMode(mode string) string {
	if mode == ModeWork {
		return ModeCoding
	}
	if mode == ModeCoding || mode == ModeChat {
		return mode
	}
	// A mode the build does not know is a project conversation's shape, which is
	// what a mode value has been for every build that wrote one.
	if mode == "" {
		return ""
	}
	return ModeCoding
}

// SessionInfo is one conversation as the task menu sees it.
type SessionInfo struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	State State  `json:"state"`
	// Mode is "coding" or "chat", which is decided by whether the conversation
	// has a project directory rather than by a setting somebody has to keep in
	// step with it. Conversations stored by earlier builds under the name "work"
	// are the same thing and are reported as coding.
	Mode      string    `json:"mode,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Project   string    `json:"project,omitempty"`
	// Execution is the level a coding conversation's next turn runs at: build,
	// plan or full access. A chat conversation has no reach to describe, so
	// the field is empty there.
	Execution string    `json:"execution,omitempty"`
	Model     string    `json:"model,omitempty"`
	Archived  bool      `json:"archived,omitempty"`
}

// Session is one conversation and everything attached to it.
type Session struct {
	Info SessionInfo `json:"info"`
	// Messages is the whole transcript, which is what a window needs to render a
	// conversation it has just opened.
	Messages []core.Message `json:"messages"`
}

// CreateRequest is what a new conversation is asked for.
type CreateRequest struct {
	// Project is the directory the conversation is about. A conversation with one
	// is a work session and lives in the project; without one it is a chat session
	// and lives in the application data directory.
	Project string `json:"project"`
	// Title is optional. A conversation nobody has spoken in is titled by its
	// first message instead.
	Title string `json:"title"`
	// Model overrides the configured default for this conversation, in
	// provider:model form. Absent means the default.
	Model string `json:"model"`
}

// SendRequest is one message sent to a conversation.
type SendRequest struct {
	Text string `json:"text"`
	// Model changes the conversation's model for this turn and the ones after it.
	Model string `json:"model"`
	// Execution is the level the turn runs at: build, plan or full access.
	// Empty is what the conversation has been running at, which is build
	// until a turn says otherwise. The turn that is sent under a mode is the
	// turn that runs under it.
	Execution string `json:"execution"`
}

// Options is how a runtime is built.
type Options struct {
	// Config is the model configuration conversations are run on. It is held
	// rather than copied, so a settings save takes effect on the next turn
	// without a restart.
	Config *config.Store
	// AppDataScope is the scope chat sessions live in. It is opened by the caller
	// rather than here so that a process with no home directory can be refused at
	// startup instead of on the first conversation.
	AppDataScope *storage.Scope
	// Gateway builds the model client for one model reference. It defaults to the
	// configuration's own builder, and it is a field because a test needs a
	// provider that answers without a server on the other end of it.
	Gateway func(config.ModelRef) (core.Provider, error)
	// NewID generates session identifiers. It is a field so a test can make
	// identifiers it can predict; nil means random.
	NewID func() (string, error)
	// Now reads the clock, for the same reason.
	Now func() time.Time
	// Logger receives what the runtime does. Optional.
	Logger func(message string, fields map[string]any)
}

// Runtime owns every conversation in this process.
type Runtime struct {
	options Options
	hub     *hub
	// ctx is the lifetime of every turn. A turn is not tied to whatever asked for
	// it: the request that carried the message finishes as soon as the message is
	// accepted, and a turn whose context was that request's would be cancelled by
	// the answer to it. The window that asked has gone; the turn has not.
	ctx     context.Context
	stopAll context.CancelFunc
	// scopes is opened once per work session and kept, because a scope holds the
	// ledger and a ledger reopened per message would be a ledger opened per
	// message.
	mu       sync.Mutex
	sessions map[string]*sessionState
	scopes   map[string]*storage.Scope
	closed   bool
}

// sessionState is one conversation's private state.
//
// The messages are duplicated from the agent's own state because the agent is
// assembled per turn and thrown away afterwards, and because a window that opens
// a conversation needs its history before any turn has run in this process.
type sessionState struct {
	info     SessionInfo
	messages []core.Message
	// runCancel stops the turn in progress. It is nil when nothing is running.
	runCancel context.CancelFunc
	// running says a turn is in progress, which is what ErrBusy is about.
	running bool
	// buffer is the durability pipeline of this session.
	buffer *storage.WriteBuffer
	// journals are the session's own files. They are held because the buffer
	// writes through them and does not own them: a file left open is a
	// conversation that cannot be deleted on the machine it lives on, which on
	// Windows means the whole directory cannot be cleaned up either.
	journals []*storage.JournalSink
	// scope is where this session's data is written.
	scope *storage.Scope
	// generation increases every time a turn starts, so an event belonging to a
	// turn that has already finished is not mistaken for one in progress.
	generation int
	// codingAssembly is the coding mode's assembly, built on the first turn and
	// kept. It holds the project's tools and its gates, and rebuilding it per
	// turn would re-walk the project and re-open the shell for nothing. It is
	// nil until a coding conversation says its first word.
	coding *mode.Coding
}

// New builds a runtime.
func New(options Options) (*Runtime, error) {
	if options.Config == nil {
		return nil, fmt.Errorf("%w: config is required", config.ErrInvalidConfig)
	}
	if options.AppDataScope == nil {
		return nil, fmt.Errorf("%w: an application data scope is required", runtimeError(""))
	}
	if options.Gateway == nil {
		// The configuration's own builder, wrapped so the field's type is the core's
		// provider rather than one particular implementation of it. A runtime that
		// knew about gateways would be a runtime that could not be given a provider
		// from anywhere else.
		store := options.Config
		options.Gateway = func(reference config.ModelRef) (core.Provider, error) {
			return store.Gateway(reference)
		}
	}
	if options.NewID == nil {
		options.NewID = newSessionID
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	runtime := &Runtime{
		options:  options,
		hub:      newHub(),
		sessions: make(map[string]*sessionState),
		scopes:   make(map[string]*storage.Scope),
	}
	runtime.ctx, runtime.stopAll = context.WithCancel(context.Background())
	if err := runtime.loadExisting(); err != nil {
		runtime.stopAll()
		return nil, err
	}
	return runtime, nil
}

func runtimeError(detail string) error {
	if detail == "" {
		return errors.New("runtime: invalid configuration")
	}
	return fmt.Errorf("runtime: %s", detail)
}

// loadExisting brings the conversations of previous runs back.
//
// It is the difference between a task menu that empties itself every restart and
// one that does not, and it is read from the journals rather than from a
// separate index so that the thing read is the thing written.
func (r *Runtime) loadExisting() error {
	scopes, err := r.discoverScopes()
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		// Every scope opened while finding conversations is one this process has
		// open, and a scope left open is a ledger left locked. They are registered
		// as they are found so Close can release them.
		r.mu.Lock()
		if _, known := r.scopes[scope.DataRoot()]; !known && scope != r.options.AppDataScope {
			r.scopes[scope.DataRoot()] = scope
		}
		r.mu.Unlock()

		summaries, err := scope.ListSessions()
		if err != nil {
			r.log("sessions did not list", map[string]any{
				"root": scope.DataRoot(), "error": err.Error(),
			})
			continue
		}
		for _, summary := range summaries {
			state, err := r.loadSession(scope, summary)
			if err != nil {
				r.log("session did not load", map[string]any{
					"session_id": summary.SessionID,
					"error":      err.Error(),
				})
				continue
			}
			r.mu.Lock()
			r.sessions[summary.SessionID] = state
			r.mu.Unlock()
		}
	}
	return nil
}

// ProjectIndexName is the file in the application data directory that records
// which projects this machine has had conversations in.
//
// It is the index a work session needs and cannot provide for itself: a work
// session's data lives inside the project, so a program that only ever looked at
// the application data directory would never know which projects to look in.
// Without it, every work conversation would vanish from the menu on restart while
// its data sat in the project untouched.
const ProjectIndexName = "projects.json"

// discoverScopes finds every place a conversation can have been left: the
// application data directory, and every project this machine has had one in.
func (r *Runtime) discoverScopes() ([]*storage.Scope, error) {
	scopes := []*storage.Scope{r.options.AppDataScope}
	seen := map[string]bool{r.options.AppDataScope.DataRoot(): true}
	for _, project := range r.knownProjects() {
		root := filepathCleanAbs(project)
		if seen[root] {
			continue
		}
		scope, err := storage.ProjectScope(root)
		if err != nil {
			// A project that has been moved or deleted is a project that is not
			// here any more. Its conversations are still on disk where it was, and
			// the failure to open it is about this machine's state rather than
			// about anything being wrong.
			r.log("project scope did not open", map[string]any{
				"project": project, "error": err.Error(),
			})
			continue
		}
		seen[root] = true
		scopes = append(scopes, scope)
	}
	return scopes, nil
}

// knownProjects reads the project index.
func (r *Runtime) knownProjects() []string {
	encoded, err := os.ReadFile(r.projectIndexPath())
	if err != nil {
		return nil
	}
	var projects []string
	if err := json.Unmarshal(encoded, &projects); err != nil {
		r.log("project index did not decode", map[string]any{"error": err.Error()})
		return nil
	}
	return projects
}

func (r *Runtime) projectIndexPath() string {
	return filepath.Join(r.options.AppDataScope.DataRoot(), ProjectIndexName)
}

// rememberProject adds a project to the index, so its conversations are found
// again after a restart.
//
// The index is written whole and atomically, and it is a convenience rather than
// a source of truth: the conversations are the journal, and an index that
// cannot be read costs the ability to find a project rather than any of them.
func (r *Runtime) rememberProject(root string) {
	if strings.TrimSpace(root) == "" {
		return
	}
	cleaned := filepathCleanAbs(root)
	projects := r.knownProjects()
	for _, existing := range projects {
		if filepathCleanAbs(existing) == cleaned {
			return
		}
	}
	projects = append(projects, cleaned)
	encoded, err := json.MarshalIndent(projects, "", "  ")
	if err != nil {
		r.log("project index did not encode", map[string]any{"error": err.Error()})
		return
	}
	path := r.projectIndexPath()
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		r.log("project index did not write", map[string]any{"error": err.Error()})
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		r.log("project index did not replace", map[string]any{"error": err.Error()})
	}
}

// loadSession rebuilds one conversation from its journal.
func (r *Runtime) loadSession(scope *storage.Scope, summary storage.SessionSummary) (*sessionState, error) {
	sessionID := summary.SessionID
	journalPath, _, err := scope.SessionJournals(sessionID)
	if err != nil {
		return nil, err
	}
	records, _, err := storage.ReadJournal(journalPath)
	if err != nil {
		return nil, err
	}
	messages := make([]core.Message, 0, len(records))
	for _, record := range records {
		if record.Type != storage.RecordMessage {
			continue
		}
		var message core.Message
		if err := json.Unmarshal(record.Data, &message); err != nil {
			// A record that cannot be read is a record the ledger already counted.
			// Skipping it keeps the rest of the conversation, and the count says
			// something was lost rather than pretending nothing was.
			r.log("journal record did not decode", map[string]any{
				"session_id": sessionID,
				"seq":        record.Seq,
				"error":      err.Error(),
			})
			continue
		}
		messages = append(messages, message)
	}
	created := summary.CreatedAt
	if created.IsZero() {
		created = r.options.Now().UTC()
	}
	updated := summary.UpdatedAt
	if updated.IsZero() {
		updated = created
	}
	state := &sessionState{
		info: SessionInfo{
			ID:        sessionID,
			Title:     summary.Summary,
			State:     StateIdle,
			Mode:      normalizeMode(summary.Mode),
			CreatedAt: created.UTC(),
			UpdatedAt: updated.UTC(),
			Project:   summary.Project,
			Execution: string(mode.ExecBuild),
			Model:     summary.Model,
		},
		messages: messages,
		scope:    scope,
	}
	// A coding conversation comes back at the level its next turn was sent
	// under. An empty value, which is what a summary from an earlier build
	// carries, is build, because that is the level a conversation has been
	// running at all along.
	if restoredMode := mode.ExecutionMode(summary.Execution); restoredMode.Valid() {
		state.info.Execution = string(restoredMode)
	}
	// A conversation that was running when the process was killed is idle as far
	// as this process is concerned, and its last turn is over even though nothing
	// said so. The messages that did arrive are still there.
	buffer, err := r.bufferFor(state)
	if err != nil {
		return nil, err
	}
	state.buffer = buffer
	return state, nil
}

// List returns every conversation, newest activity first.
func (r *Runtime) List() []SessionInfo {
	r.mu.Lock()
	sessions := make([]*sessionState, 0, len(r.sessions))
	for _, session := range r.sessions {
		sessions = append(sessions, session)
	}
	r.mu.Unlock()

	infos := make([]SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		infos = append(infos, r.snapshotInfo(session))
	}
	sortSessions(infos)
	return infos
}

func (r *Runtime) snapshotInfo(session *sessionState) SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	info := session.info
	if session.running {
		info.State = StateRunning
	} else if info.State == StateRunning {
		// A session left marked running by a process that was killed mid turn is
		// idle as far as this process is concerned: nothing is running in it.
		info.State = StateIdle
	}
	return info
}

func sortSessions(infos []SessionInfo) {
	sort.SliceStable(infos, func(left, right int) bool {
		return infos[left].UpdatedAt.After(infos[right].UpdatedAt)
	})
}

// Get returns one conversation with its messages.
func (r *Runtime) Get(sessionID string) (Session, error) {
	session, err := r.lookup(sessionID)
	if err != nil {
		return Session{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	messages := make([]core.Message, len(session.messages))
	copy(messages, session.messages)
	// The slice is never nil, for the reason Messages gives: a window that
	// received null has to decide what to do about it.
	return Session{Info: r.infoLocked(session), Messages: messages}, nil
}

// Messages returns one conversation's messages.
//
// The slice is never nil, because a window that received null has to decide what
// to do about it and an empty array is already the answer.
func (r *Runtime) Messages(sessionID string) ([]core.Message, error) {
	session, err := r.lookup(sessionID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	messages := make([]core.Message, len(session.messages))
	copy(messages, session.messages)
	return messages, nil
}

func (r *Runtime) lookup(sessionID string) (*sessionState, error) {
	if err := storage.ValidateID("session id", sessionID); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	session, exists := r.sessions[sessionID]
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrNoSession, sessionID)
	}
	return session, nil
}

// infoLocked renders the session's information. The caller holds the lock.
func (r *Runtime) infoLocked(session *sessionState) SessionInfo {
	info := session.info
	if session.running {
		info.State = StateRunning
	}
	return info
}

// Create opens a conversation.
//
// It does not assemble an agent. A conversation that exists and has said nothing
// has no model call in it, and a window that opens the settings page and comes
// back should find the conversation it left rather than an error from a
// provider that was never asked anything.
func (r *Runtime) Create(request CreateRequest) (SessionInfo, error) {
	if r.options.NewID == nil {
		return SessionInfo{}, runtimeError("no identifier source")
	}
	sessionID, err := r.options.NewID()
	if err != nil {
		return SessionInfo{}, fmt.Errorf("runtime: generate session ID: %w", err)
	}
	if err := storage.ValidateID("session id", sessionID); err != nil {
		return SessionInfo{}, err
	}
	project := strings.TrimSpace(request.Project)
	scope, err := r.scopeFor(project)
	if err != nil {
		return SessionInfo{}, err
	}
	now := r.options.Now().UTC()
	reference, refErr := r.options.Config.Resolve(request.Model)
	spec := ""
	if refErr == nil {
		spec = reference.Spec()
	}
	session := &sessionState{
		info: SessionInfo{
			ID:        sessionID,
			Title:     strings.TrimSpace(request.Title),
			State:     StateIdle,
			CreatedAt: now,
			UpdatedAt: now,
			Project:   project,
			Model:     spec,
		},
		scope: scope,
	}
	if project != "" {
		session.info.Mode = ModeCoding
		session.info.Execution = string(mode.ExecBuild)
	} else {
		session.info.Mode = ModeChat
	}
	buffer, err := r.bufferFor(session)
	if err != nil {
		return SessionInfo{}, err
	}
	session.buffer = buffer

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return SessionInfo{}, ErrClosed
	}
	r.sessions[sessionID] = session
	r.mu.Unlock()

	// The project is remembered before the conversation is announced, so a
	// conversation that is listed after a restart is one that can be found.
	if project != "" {
		r.rememberProject(project)
	}
	if err := r.persistSummary(session); err != nil {
		return SessionInfo{}, err
	}
	r.hub.publishState(r.infoSnapshot(session))
	return r.infoSnapshot(session), nil
}

func (r *Runtime) infoSnapshot(session *sessionState) SessionInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.infoLocked(session)
}

// projectScopeKey is the map key a project's scope is registered and looked up
// by: the project's data directory, which is where its ledger and its lock are.
//
// The key is the data root rather than the project root because that is what
// the startup registers scopes by when it brings the conversations of previous
// runs back. Two spellings of one directory were two keys, and a conversation
// created after a restart would then open the same ledger again and be refused
// by the lock this process itself was holding.
func projectScopeKey(project string) string {
	return filepathCleanAbs(filepath.Join(filepathCleanAbs(project), storage.ProjectDirName))
}

// scopeFor picks where a conversation's data is written.
//
// A conversation with a directory is a coding session and its data goes in that
// directory, beside the code it describes, so that somebody auditing the project
// finds the conversation. One without is a chat session and its data goes in the
// application data directory, so that it does not depend on a project existing.
func (r *Runtime) scopeFor(project string) (*storage.Scope, error) {
	if project == "" {
		return r.options.AppDataScope, nil
	}
	key := projectScopeKey(project)
	r.mu.Lock()
	existing, exists := r.scopes[key]
	r.mu.Unlock()
	if exists {
		return existing, nil
	}
	scope, err := storage.ProjectScope(project)
	if err != nil {
		return nil, fmt.Errorf("runtime: open project scope: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		_ = scope.Close()
		return nil, ErrClosed
	}
	if existing, exists := r.scopes[key]; exists {
		// Two conversations for the same project started at once. One of them
		// opened the scope and the other has to use that one rather than the
		// second copy, because two ledgers for one project is two truths.
		_ = scope.Close()
		return existing, nil
	}
	r.scopes[key] = scope
	return scope, nil
}

// bufferFor builds the durability pipeline of one session, once.
//
// The two journals and the ledger are all fed by one buffer, so a record cannot
// be in the ledger and missing from the file a person can read, and a record
// cannot be in one file and missing from the other. The split sink is what puts
// each record in the file its own stream belongs to rather than in both.
func (r *Runtime) bufferFor(session *sessionState) (*storage.WriteBuffer, error) {
	sessionJournal, eventsJournal, err := session.scope.OpenSessionJournals(session.info.ID)
	if err != nil {
		return nil, fmt.Errorf("runtime: open session journals: %w", err)
	}
	split, err := storage.NewSplitSink(map[string]storage.Sink{
		storage.StreamSession: sessionJournal,
		storage.StreamEvents:  eventsJournal,
	}, session.scope.Audit(), indexableMessage)
	if err != nil {
		_ = sessionJournal.Close()
		_ = eventsJournal.Close()
		return nil, err
	}
	buffer, err := storage.New(storage.Config{}, split)
	if err != nil {
		_ = sessionJournal.Close()
		_ = eventsJournal.Close()
		return nil, err
	}
	session.journals = append(session.journals, sessionJournal, eventsJournal)
	return buffer, nil
}

// Close stops every turn and flushes every pipeline.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	sessions := make([]*sessionState, 0, len(r.sessions))
	for _, session := range r.sessions {
		sessions = append(sessions, session)
	}
	scopes := make([]*storage.Scope, 0, len(r.scopes)+1)
	scopes = append(scopes, r.options.AppDataScope)
	for _, scope := range r.scopes {
		scopes = append(scopes, scope)
	}
	r.mu.Unlock()

	// Every turn in the process ends here, before the flush, so that a turn still
	// producing records is not racing the last write that is supposed to contain
	// it.
	r.stopAll()
	for _, session := range sessions {
		r.cancelRun(session)
	}
	// The budget belongs here rather than to each buffer, so a slow disk costs
	// one bounded wait instead of one per conversation.
	var firstErr error
	for _, session := range sessions {
		if err := r.persistSummary(session); err != nil && firstErr == nil {
			firstErr = err
		}
		// The coding mode's own parts end with the conversation: the wiki
		// index, the language server and the sub agent pool each release
		// their hold here, after the turns are gone and before the scope is.
		if session.coding != nil {
			if err := session.coding.Close(ctx); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("runtime: close the coding assembly of %s: %w", session.info.ID, err)
			}
		}
		if session.buffer == nil {
			continue
		}
		if err := session.buffer.Close(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("runtime: flush session %s: %w", session.info.ID, err)
		}
		// The journals are closed after the flush rather than before, because a
		// record still queued is a record the file has not seen.
		for _, journal := range session.journals {
			if err := journal.Close(); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("runtime: close session %s: %w", session.info.ID, err)
			}
		}
	}
	r.hub.close()
	for _, scope := range scopes {
		if err := scope.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *Runtime) cancelRun(session *sessionState) {
	r.mu.Lock()
	cancel := session.runCancel
	session.runCancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *Runtime) log(message string, fields map[string]any) {
	if r.options.Logger == nil {
		return
	}
	r.options.Logger(message, fields)
}

// Subscribe hands the event stream to the web server.
//
// It is the runtime rather than the hub that the web package is given, so that
// the stream and the conversations it describes are behind one door: a window
// asking to watch a conversation this runtime has never heard of is refused,
// rather than being answered with a stream that will never speak and a panel
// that will therefore look empty rather than broken.
func (r *Runtime) Subscribe(ctx context.Context, sessionID string, after int64) (<-chan web.Event, func(), error) {
	if _, err := r.lookup(sessionID); err != nil {
		return nil, nil, err
	}
	return r.hub.Subscribe(ctx, sessionID, after)
}

// Watchers reports how many windows are watching a conversation.
func (r *Runtime) Watchers(sessionID string) int { return r.hub.Watchers(sessionID) }

// ProviderCatalog is the set of providers this build knows about, for a page
// offering to add one without typing an address.
func (r *Runtime) ProviderCatalog() []config.Preset { return config.Presets() }

// ProviderTest is the answer to "does this one work", asked of the endpoint
// rather than guessed at.
//
// It sends the smallest request that can be answered: one word, and the smallest
// number of tokens that still is an answer. Anything less would be a request the
// provider could accept and then refuse to say anything, which would look like
// success on a machine where nothing is actually configured.
//
// The provider comes in as a spec rather than as a name, because the person
// pressing this has usually just typed the address and has not saved it yet. A
// probe that only worked on saved providers would fail exactly when it is most
// wanted.
//
// The result is reported rather than returned as an error, because a failure is
// an answer here rather than a fault: a page asking whether a key works has to be
// able to show what went wrong next to the field that is wrong, and an error
// would either be swallowed or shown as a failure of the page.
func (r *Runtime) ProviderTest(ctx context.Context, spec config.ProviderSpec, model string) TestResult {
	started := time.Now()
	result := TestResult{Provider: spec.Name, Model: model}
	if strings.TrimSpace(model) == "" {
		result.Error = "没有指定模型"
		return result
	}
	built, err := r.options.Config.GatewayFor(spec, model)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	var declared config.Model
	if stored, exists := r.options.Config.Provider(spec.Name); exists {
		declared, _ = stored.FindModel(model)
	}

	// The request is bounded twice over: by the context the caller gave, and by a
	// budget of its own, because a probe that waits as long as a conversation
	// would be a probe nobody waits for.
	probeCtx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	stream, err := built.ChatStream(probeCtx, core.ChatRequest{
		Model:     model,
		MaxTokens: 1,
		Messages: []core.Message{{
			ID:      "probe",
			Role:    core.RoleUser,
			Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "hi"}},
		}},
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	var failure error
	for chunk := range stream {
		if chunk.Err != nil {
			failure = chunk.Err
			break
		}
		if chunk.Type == core.StreamTypeError {
			failure = errors.New(chunk.Text)
			break
		}
	}
	if failure != nil {
		result.Error = failure.Error()
		return result
	}
	result.OK = true
	result.Latency = time.Since(started)
	result.LatencyMS = result.Latency.Milliseconds()
	if declared.ContextWindow > 0 {
		result.ContextWindow = declared.ContextWindow
	}
	return result
}

// probeBudget bounds one probe. It is longer than a request should take and far
// shorter than a conversation, because a probe that has not answered in this
// time is an endpoint that is not going to.
const probeBudget = 20 * time.Second

// TestResult is what a probe found.
type TestResult struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	OK       bool   `json:"ok"`
	// Latency is how long the answer took, which is the number somebody watching
	// a spinning button actually wants: an endpoint that answers in 80ms and one
	// that answers in 8s are both working and are not the same thing to use.
	Latency time.Duration `json:"-"`
	// LatencyMS is the same figure in the unit a page shows.
	LatencyMS     int64  `json:"latency_ms,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	Error         string `json:"error,omitempty"`
}

// ProviderModels asks one configured provider which models it offers.
//
// It is here rather than in the interface because it needs the provider's
// credentials, and those live in the configuration. A window asking what a
// provider offers is asking a question about this machine's setup, and the
// answer has to be assembled where the answer is.
func (r *Runtime) ProviderModels(ctx context.Context, spec config.ProviderSpec) ([]gateway.Model, error) {
	// Which credential the request will actually end up using is a question about
	// the stored provider as well as about the request, so it is answered from
	// both: a spec with its own key uses that, and one without falls back to what
	// is stored under the name it gave.
	key := spec.APIKey
	keyEnv := spec.APIKeyEnv
	if stored, exists := r.options.Config.Provider(spec.Name); exists {
		if key == "" {
			key = stored.APIKey
		}
		if keyEnv == "" {
			keyEnv = stored.APIKeyEnv
		}
	}
	// A provider whose key is in a variable that is not set is reported before the
	// request rather than as whatever the endpoint says about an unauthenticated
	// request, because the two have different fixes and only one of them is "set
	// the variable".
	if key == "" && keyEnv != "" && os.Getenv(keyEnv) == "" {
		return nil, fmt.Errorf(
			"%w: provider %q has no key: the environment variable %s is not set",
			gateway.ErrModelList, spec.Name, keyEnv)
	}
	// The reference is only the carrier for the endpoint and the format; the model
	// in it is not asked about.
	built, err := r.options.Config.GatewayFor(spec, "unused")
	if err != nil {
		return nil, err
	}
	models, err := built.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", spec.Name, err)
	}
	if models == nil {
		models = []gateway.Model{}
	}
	return models, nil
}

// newSessionID makes an identifier for a conversation.
//
// A timestamp first and randomness after it, so the directory a conversation
// writes into sorts in the order the conversations were started, which is the
// order a person reading the list is looking for. The random part is what makes
// two conversations started in the same second distinguishable.
func newSessionID() (string, error) {
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("runtime: generate session ID: %w", err)
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(random[:]), nil
}

// filepathCleanAbs is the path a session's project is keyed by.
//
// Cleaned and made absolute, because "D:\work" and "D:\work\" and
// "d:\Work" are one directory on Windows and three keys here, and three scopes
// for one project is three ledgers for one project.
func filepathCleanAbs(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return absolute
}

var _ web.Stream = (*Runtime)(nil)
