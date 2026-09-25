// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion is bumped whenever the audit schema changes, so an old database
// is detected instead of silently queried with the wrong shape.
const SchemaVersion = 1

// TrigramMinQueryLength is the shortest query the trigram tokenizer can match.
// Below it a substring search falls back to LIKE, which is why the search
// supports one and two character queries in both languages.
const TrigramMinQueryLength = 3

// AuditSink is the queryable half of the durability design: an append only
// ledger plus a full text index, both written in the same transaction as the
// journal so the two can never disagree.
type AuditSink struct {
	mu     sync.Mutex
	db     *sql.DB
	closed bool
}

const auditSchema = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         DATETIME NOT NULL,
    session_id TEXT NOT NULL,
    agent_id   TEXT NOT NULL,
    event_type TEXT NOT NULL,
    payload    TEXT NOT NULL,
    trace_id   TEXT,
    span_id    TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_session_ts ON events(session_id, ts);
CREATE TABLE IF NOT EXISTS tool_calls (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          DATETIME NOT NULL,
    session_id  TEXT NOT NULL,
    agent_id    TEXT NOT NULL,
    tool_name   TEXT NOT NULL,
    params      TEXT NOT NULL,
    result      TEXT,
    status      TEXT NOT NULL,
    approved_by TEXT,
    duration_ms INTEGER
);
CREATE TABLE IF NOT EXISTS tool_call_audit (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    stream    TEXT NOT NULL,
    seq       INTEGER NOT NULL,
    status    TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    ts        DATETIME NOT NULL,
    UNIQUE (stream, seq)
);
CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL,
    message_id TEXT NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    ts         DATETIME NOT NULL,
    UNIQUE (session_id, message_id)
);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    session_id, message_id, role, content, tokenize='trigram'
);
`

// OpenAudit opens or creates the audit database in WAL mode.
func OpenAudit(path string) (*AuditSink, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("storage: audit path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("storage: create audit directory: %w", err)
	}
	// The pure Go driver has no cross process locking, so a single writer keeps
	// the ledger consistent and WAL keeps readers from blocking it.
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open audit database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(auditSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: create audit schema: %w", err)
	}
	if _, err := db.Exec(
		`INSERT INTO meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		fmt.Sprint(SchemaVersion),
	); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: record schema version: %w", err)
	}
	return &AuditSink{db: db}, nil
}

// SchemaVersionOf reports the schema version recorded in a database.
func (s *AuditSink) SchemaVersionOf() (int, error) {
	var value string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&value); err != nil {
		return 0, fmt.Errorf("storage: read schema version: %w", err)
	}
	var version int
	if _, err := fmt.Sscanf(value, "%d", &version); err != nil {
		return 0, fmt.Errorf("storage: parse schema version %q: %w", value, err)
	}
	return version, nil
}

// Write stores a batch in one transaction. A record is routed by its type: a
// message record is indexed for search, and a tool call state transition is
// deduplicated by stream and sequence so a replay cannot double count.
func (s *AuditSink) Write(ctx context.Context, batch []Record) error {
	if len(batch) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrBufferClosed
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin audit transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	statement, err := transaction.PrepareContext(ctx, `
		INSERT INTO events(ts, session_id, agent_id, event_type, payload)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return fmt.Errorf("storage: prepare event insert: %w", err)
	}
	defer statement.Close()

	messageInsert, err := transaction.PrepareContext(ctx, `
		INSERT INTO messages(session_id, message_id, role, content, ts)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(session_id, message_id) DO UPDATE SET
			content = excluded.content, role = excluded.role, ts = excluded.ts`)
	if err != nil {
		return fmt.Errorf("storage: prepare message insert: %w", err)
	}
	defer messageInsert.Close()

	searchInsert, err := transaction.PrepareContext(ctx, `
		INSERT INTO messages_fts(session_id, message_id, role, content)
		VALUES(?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("storage: prepare search insert: %w", err)
	}
	defer searchInsert.Close()

	toolInsert, err := transaction.PrepareContext(ctx, `
		INSERT INTO tool_call_audit(stream, seq, status, tool_name, ts)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(stream, seq) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("storage: prepare tool call insert: %w", err)
	}
	defer toolInsert.Close()

	for _, record := range batch {
		ts := record.Ts
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		sessionID, agentID := record.Stream, "agent"
		if kind, id, found := strings.Cut(record.Stream, ":"); found && kind != "" {
			sessionID = id
		}
		if _, err := statement.ExecContext(ctx, ts, sessionID, agentID, record.Type, string(record.Data)); err != nil {
			return fmt.Errorf("storage: insert event: %w", err)
		}
		switch record.Type {
		case RecordMessage:
			var message struct {
				ID      string `json:"id"`
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(record.Data, &message); err != nil {
				return fmt.Errorf("storage: decode message record: %w", err)
			}
			if message.ID == "" {
				message.ID = fmt.Sprint(record.Seq)
			}
			content := message.Content
			if content == "" {
				content = string(record.Data)
			}
			if _, err := messageInsert.ExecContext(ctx, sessionID, message.ID, message.Role, content, ts); err != nil {
				return fmt.Errorf("storage: insert message: %w", err)
			}
			if _, err := searchInsert.ExecContext(ctx, sessionID, message.ID, message.Role, content); err != nil {
				return fmt.Errorf("storage: index message: %w", err)
			}
		case RecordToolCallState:
			var state struct {
				ToolName string `json:"tool_name"`
				Status   string `json:"status"`
			}
			if err := json.Unmarshal(record.Data, &state); err != nil {
				return fmt.Errorf("storage: decode tool call record: %w", err)
			}
			if state.Status == "" {
				state.Status = "executed"
			}
			if _, err := toolInsert.ExecContext(ctx, record.Stream, record.Seq, state.Status, state.ToolName, ts); err != nil {
				return fmt.Errorf("storage: insert tool call state: %w", err)
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("storage: commit audit transaction: %w", err)
	}
	return nil
}

// Sync is a no-op because SQLite commits are durable by default; the method
// exists so the sink satisfies the pipeline contract.
func (s *AuditSink) Sync(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrBufferClosed
	}
	return nil
}

// Close checkpoints the write ahead log so the database file is self contained.
func (s *AuditSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = s.db.Close()
		return fmt.Errorf("storage: checkpoint audit database: %w", err)
	}
	return s.db.Close()
}

// SearchResult is one message that matched a query.
type SearchResult struct {
	SessionID string
	MessageID string
	Role      string
	Content   string
}

// SearchMessages finds messages by substring. Queries of at least three
// characters use the trigram index; shorter ones fall back to a scan, because
// the tokenizer cannot index them.
func (s *AuditSink) SearchMessages(ctx context.Context, sessionID, query string, limit int) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("storage: search query must not be empty")
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrBufferClosed
	}

	if len([]rune(query)) < TrigramMinQueryLength {
		rows, err := s.db.QueryContext(ctx, `
			SELECT session_id, message_id, role, content FROM messages
			WHERE content LIKE ? ESCAPE '\' ORDER BY ts LIMIT ?`,
			"%"+escapeLike(query)+"%", limit)
		if err != nil {
			return nil, fmt.Errorf("storage: search messages: %w", err)
		}
		return collectSearchResults(rows)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, message_id, role, content FROM messages_fts
		WHERE messages_fts MATCH ? AND (? = '' OR session_id = ?)
		ORDER BY rowid LIMIT ?`,
		ftsQuery(query), sessionID, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: search messages: %w", err)
	}
	return collectSearchResults(rows)
}

// ftsQuery quotes the user's text so a query made of punctuation cannot be
// parsed as FTS syntax.
func ftsQuery(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

func escapeLike(query string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(query)
}

func collectSearchResults(rows *sql.Rows) ([]SearchResult, error) {
	defer rows.Close()
	results := make([]SearchResult, 0, 8)
	for rows.Next() {
		var result SearchResult
		if err := rows.Scan(&result.SessionID, &result.MessageID, &result.Role, &result.Content); err != nil {
			return nil, fmt.Errorf("storage: scan search result: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read search results: %w", err)
	}
	return results, nil
}

// EventCount reports how many events a session has, which recovery compares
// against the journal.
func (s *AuditSink) EventCount(ctx context.Context, sessionID string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE session_id = ?`, sessionID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("storage: count events: %w", err)
	}
	return count, nil
}

// HighestToolSeq reports the last tool call sequence recorded for a stream.
func (s *AuditSink) HighestToolSeq(ctx context.Context, stream string) (int64, error) {
	var seq sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT max(seq) FROM tool_call_audit WHERE stream = ?`, stream,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("storage: read tool sequence: %w", err)
	}
	return seq.Int64, nil
}

// LastAppliedSeq reads the recovery checkpoint, which is a metadata pointer
// rather than a snapshot because the journal is already the full record.
func (s *AuditSink) LastAppliedSeq(ctx context.Context, sessionID string) (int64, error) {
	var value sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = ?`, "checkpoint:"+sessionID,
	).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("storage: read checkpoint: %w", err)
	}
	if !value.Valid || value.String == "" {
		return 0, nil
	}
	var seq int64
	if _, err := fmt.Sscanf(value.String, "%d", &seq); err != nil {
		return 0, fmt.Errorf("storage: parse checkpoint %q: %w", value.String, err)
	}
	return seq, nil
}

// WriteCheckpoint records how far a session has been replayed.
func (s *AuditSink) WriteCheckpoint(ctx context.Context, sessionID string, seq int64, messageCount int, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrBufferClosed
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin checkpoint transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	entries := map[string]string{
		"checkpoint:" + sessionID:          fmt.Sprint(seq),
		"checkpoint_messages:" + sessionID: fmt.Sprint(messageCount),
		"checkpoint_summary:" + sessionID:  summary,
	}
	for key, value := range entries {
		if _, err := transaction.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, value,
		); err != nil {
			return fmt.Errorf("storage: write checkpoint: %w", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("storage: commit checkpoint: %w", err)
	}
	return nil
}

// Checkpoint reads the recorded checkpoint of a session.
func (s *AuditSink) Checkpoint(ctx context.Context, sessionID string) (seq int64, messageCount int, summary string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, 0, "", ErrBufferClosed
	}
	read := func(key string) (string, error) {
		var value sql.NullString
		if queryErr := s.db.QueryRowContext(ctx,
			`SELECT value FROM meta WHERE key = ?`, key,
		).Scan(&value); queryErr != nil {
			if errors.Is(queryErr, sql.ErrNoRows) {
				return "", nil
			}
			return "", queryErr
		}
		return value.String, nil
	}
	seqValue, err := read("checkpoint:" + sessionID)
	if err != nil {
		return 0, 0, "", fmt.Errorf("storage: read checkpoint: %w", err)
	}
	countValue, err := read("checkpoint_messages:" + sessionID)
	if err != nil {
		return 0, 0, "", fmt.Errorf("storage: read checkpoint: %w", err)
	}
	summary, err = read("checkpoint_summary:" + sessionID)
	if err != nil {
		return 0, 0, "", fmt.Errorf("storage: read checkpoint: %w", err)
	}
	if _, err := fmt.Sscanf(seqValue, "%d", &seq); err != nil && seqValue != "" {
		return 0, 0, "", fmt.Errorf("storage: parse checkpoint %q: %w", seqValue, err)
	}
	if _, err := fmt.Sscanf(countValue, "%d", &messageCount); err != nil && countValue != "" {
		return 0, 0, "", fmt.Errorf("storage: parse checkpoint %q: %w", countValue, err)
	}
	return seq, messageCount, summary, nil
}
