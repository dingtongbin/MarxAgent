// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ScopeKind distinguishes the two storage scopes. Chat sessions live with the
// application data so they survive a project being moved or deleted; work
// sessions live inside the project so the data is auditable next to the code
// it describes.
type ScopeKind string

const (
	// ScopeAppData is the application data scope used by chat mode.
	ScopeAppData ScopeKind = "appdata"
	// ScopeProject is the project scope used by work mode.
	ScopeProject ScopeKind = "project"
)

// ProjectDirName is the per project data directory inside a repository.
const ProjectDirName = ".marx"

// DBFileName is the audit database inside a scope root.
const DBFileName = "marxagent.db"

// SessionsDirName holds one directory per session.
const SessionsDirName = "sessions"

// BrokerDirName holds the sub agent message journal.
const BrokerDirName = "broker"

// BrokerJournalName is the journal file of the broker.
const BrokerJournalName = "journal"

// WikiDirName holds the project knowledge base.
const WikiDirName = "wiki"

// LSPCacheName is the language server cache inside a project scope.
const LSPCacheName = "cache.json"

// LogsDirName holds rolling log files.
const LogsDirName = "logs"

// AppDirName is the MarxAgent directory inside the user configuration
// directory.
const AppDirName = "marxagent"

// Scope is a storage root with the layout the design specifies:
//
//	sessions/<session id>/session.jsonl   message history, human readable
//	sessions/<session id>/events.jsonl    raw event stream
//	broker/journal.jsonl                  sub agent messages
//	marxagent.db                          audit ledger
type Scope struct {
	kind ScopeKind
	root string
	// dataRoot is where sessions and journals live, which is the project
	// directory itself for a project scope.
	dataRoot string
	audit    *AuditSink
	lock     *FileLock
}

// OpenScope prepares a scope, creating the layout and the ledger.
func OpenScope(kind ScopeKind, root string) (*Scope, error) {
	switch kind {
	case ScopeAppData, ScopeProject:
	default:
		return nil, fmt.Errorf("storage: unknown scope kind %q", kind)
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("storage: scope root must not be empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve scope root: %w", err)
	}
	dataRoot := absolute
	if kind == ScopeAppData {
		dataRoot = absolute
	}
	for _, directory := range []string{
		filepath.Join(dataRoot, SessionsDirName),
		filepath.Join(dataRoot, BrokerDirName),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return nil, fmt.Errorf("storage: create scope directory: %w", err)
		}
	}
	if kind == ScopeProject {
		if err := os.MkdirAll(filepath.Join(dataRoot, WikiDirName), 0o755); err != nil {
			return nil, fmt.Errorf("storage: create wiki directory: %w", err)
		}
	}
	audit, err := OpenAudit(filepath.Join(dataRoot, DBFileName))
	if err != nil {
		return nil, err
	}
	lock, err := LockFile(filepath.Join(dataRoot, DBFileName+".lock"))
	if err != nil {
		_ = audit.Close()
		return nil, err
	}
	return &Scope{
		kind:     kind,
		root:     absolute,
		dataRoot: dataRoot,
		audit:    audit,
		lock:     lock,
	}, nil
}

// AppDataScope opens the application data scope under the user configuration
// directory, which is how a chat session avoids depending on any project.
func AppDataScope() (*Scope, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("storage: locate user config directory: %w", err)
	}
	return OpenScope(ScopeAppData, filepath.Join(directory, AppDirName))
}

// ProjectScope opens the project scope under a repository root.
func ProjectScope(root string) (*Scope, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve project root: %w", err)
	}
	return OpenScope(ScopeProject, filepath.Join(absolute, ProjectDirName))
}

// Kind reports which scope this is.
func (s *Scope) Kind() ScopeKind { return s.kind }

// Root reports the scope root.
func (s *Scope) Root() string { return s.root }

// DataRoot reports where sessions, journals and the ledger live.
func (s *Scope) DataRoot() string { return s.dataRoot }

// Audit exposes the ledger.
func (s *Scope) Audit() *AuditSink { return s.audit }

// SessionsDir reports the directory holding session directories.
func (s *Scope) SessionsDir() string { return filepath.Join(s.dataRoot, SessionsDirName) }

// WikiDir reports the knowledge base directory, which only a project scope has.
func (s *Scope) WikiDir() string { return filepath.Join(s.dataRoot, WikiDirName) }

// LogsDir reports where rolling logs are written.
func (s *Scope) LogsDir() string { return filepath.Join(s.dataRoot, LogsDirName) }

// LSPCachePath reports the language server cache path.
func (s *Scope) LSPCachePath() string { return filepath.Join(s.dataRoot, LSPCacheName) }

// SessionDir reports the directory of one session.
func (s *Scope) SessionDir(sessionID string) (string, error) {
	if err := ValidateID("session id", sessionID); err != nil {
		return "", err
	}
	return filepath.Join(s.SessionsDir(), sessionID), nil
}

// BrokerJournalPath reports the sub agent message journal.
func (s *Scope) BrokerJournalPath() string {
	return filepath.Join(s.dataRoot, BrokerDirName, BrokerJournalName+".jsonl")
}

// SessionJournals reports the two journals of a session in a stable order.
func (s *Scope) SessionJournals(sessionID string) (sessionPath, eventsPath string, err error) {
	directory, err := s.SessionDir(sessionID)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(directory, StreamSession+".jsonl"),
		filepath.Join(directory, StreamEvents+".jsonl"), nil
}

// SessionStreams reports the streams of a session in a stable order.
func (s *Scope) SessionStreams(sessionID string) []string {
	return []string{StreamSession + ":" + sessionID, StreamEvents + ":" + sessionID}
}

// EnsureSessionDir creates the directory of a session.
func (s *Scope) EnsureSessionDir(sessionID string) (string, error) {
	directory, err := s.SessionDir(sessionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("storage: create session directory: %w", err)
	}
	return directory, nil
}

// OpenSessionJournals opens both journals of a session.
func (s *Scope) OpenSessionJournals(sessionID string) (*JournalSink, *JournalSink, error) {
	if _, err := s.EnsureSessionDir(sessionID); err != nil {
		return nil, nil, err
	}
	sessionPath, eventsPath, err := s.SessionJournals(sessionID)
	if err != nil {
		return nil, nil, err
	}
	sessionJournal, err := OpenJournal(sessionPath)
	if err != nil {
		return nil, nil, err
	}
	eventsJournal, err := OpenJournal(eventsPath)
	if err != nil {
		_ = sessionJournal.Close()
		return nil, nil, err
	}
	return sessionJournal, eventsJournal, nil
}

// OpenBrokerJournal opens the sub agent message journal.
func (s *Scope) OpenBrokerJournal() (*JournalSink, error) {
	return OpenJournal(s.BrokerJournalPath())
}

// Marker returns the clean shutdown marker of the scope.
func (s *Scope) Marker() (*CleanMarker, error) {
	return NewCleanMarker(s.dataRoot, s.audit)
}

// Lock returns the exclusive lock of the scope. Holding it is what keeps two
// processes from writing the same ledger.
func (s *Scope) Lock() *FileLock { return s.lock }

// Recover repairs and replays every journal in the scope.
func (s *Scope) Recover(ctx context.Context) (RecoveryResult, error) {
	return Recover(ctx, s.dataRoot, s.audit)
}

// Checkpoint records how far a session's message stream has been replayed.
func (s *Scope) Checkpoint(ctx context.Context, sessionID string, seq int64, messageCount int, summary string) error {
	return s.audit.WriteCheckpoint(ctx, StreamSession+":"+sessionID, seq, messageCount, summary)
}

// CheckpointStream records how far any stream has been replayed.
func (s *Scope) CheckpointStream(ctx context.Context, stream string, seq int64) error {
	return s.audit.WriteCheckpoint(ctx, stream, seq, 0, "")
}

// Close releases the lock and the ledger.
func (s *Scope) Close() error {
	var firstErr error
	if s.lock != nil {
		if err := s.lock.Release(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.audit != nil {
		if err := s.audit.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SessionSummary is the metadata a session directory carries alongside its
// journals, so a session can be listed without opening the ledger.
type SessionSummary struct {
	SessionID    string    `json:"session_id"`
	Mode         string    `json:"mode"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
	LastSeq      int64     `json:"last_seq"`
	Summary      string    `json:"summary,omitempty"`
}

// SummaryFileName is the metadata file inside a session directory.
const SummaryFileName = "summary.json"

// WriteSessionSummary stores the session metadata.
func (s *Scope) WriteSessionSummary(summary SessionSummary) error {
	directory, err := s.EnsureSessionDir(summary.SessionID)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("storage: encode session summary: %w", err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join(directory, SummaryFileName)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("storage: write session summary: %w", err)
	}
	return nil
}

// ReadSessionSummary loads the session metadata, reporting absence rather than
// an error because a session directory without a summary is normal early on.
func (s *Scope) ReadSessionSummary(sessionID string) (SessionSummary, error) {
	directory, err := s.SessionDir(sessionID)
	if err != nil {
		return SessionSummary{}, err
	}
	encoded, err := os.ReadFile(filepath.Join(directory, SummaryFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return SessionSummary{}, nil
		}
		return SessionSummary{}, fmt.Errorf("storage: read session summary: %w", err)
	}
	var summary SessionSummary
	if err := json.Unmarshal(encoded, &summary); err != nil {
		return SessionSummary{}, fmt.Errorf("storage: decode session summary: %w", err)
	}
	return summary, nil
}

// ListSessions returns the sessions of the scope ordered by identifier.
func (s *Scope) ListSessions() ([]SessionSummary, error) {
	entries, err := os.ReadDir(s.SessionsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("storage: read sessions: %w", err)
	}
	summaries := make([]SessionSummary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		summary, err := s.ReadSessionSummary(entry.Name())
		if err != nil {
			return nil, err
		}
		if summary.SessionID == "" {
			summary.SessionID = entry.Name()
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// ValidateID rejects identifiers that would escape their directory. A session
// id reaches the scope from configuration and from a request, so it is never
// trusted as a path component.
func ValidateID(label, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("storage: %s must not be empty", label)
	}
	if id == "." || id == ".." || strings.ContainsAny(id, `/\:`) {
		return fmt.Errorf("storage: %s %q is not a valid identifier", label, id)
	}
	if strings.ContainsRune(id, 0) {
		return fmt.Errorf("storage: %s %q contains a null byte", label, id)
	}
	return nil
}
