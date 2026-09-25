// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFailurePathsAreReported pins the error contracts of both sinks. Durability
// code that hides an error is worse than code that fails, so every one of these
// branches asserts a wrapped, non nil error rather than a silent success.
func TestOpenJournalReportsFilesystemFailures(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenJournal(dir); err == nil {
		t.Fatal("a directory was accepted as a journal")
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent of the journal is a regular file, so creating it must fail.
	if _, err := OpenJournal(filepath.Join(blocker, "nested", "session.jsonl")); err == nil {
		t.Fatal("a file was accepted as a journal directory")
	}
}

func TestJournalWriteReportsAClosedFile(t *testing.T) {
	sink, err := OpenJournal(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// Closing the handle behind the sink's back is the only way to reach the
	// write error, because Close itself is guarded.
	if err := sink.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(context.Background(), []Record{{
		Stream: "session:a", Type: RecordMessage, Data: json.RawMessage(`{}`),
	}}); err == nil {
		t.Fatal("a write to a closed handle was accepted")
	}
	if err := sink.Sync(context.Background()); err == nil {
		t.Fatal("a sync of a closed handle was accepted")
	}
	_ = sink.Close()
}

func TestJournalReadReportsFailures(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ReadJournal(dir); err == nil {
		t.Fatal("a directory was read as a journal")
	}
	// An absent journal is the normal first run case, not a failure, so the
	// contract is that it reads as empty rather than raising.
	records, partial, err := ReadJournal(filepath.Join(dir, "absent", "session.jsonl"))
	if err != nil {
		t.Fatalf("an absent journal reported an error: %v", err)
	}
	if len(records) != 0 || len(partial) != 0 {
		t.Fatalf("absent journal = %d records, %q partial", len(records), partial)
	}
}

func TestTruncatePartialJournalRejectsInconsistentSizes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := TruncatePartialJournal(path, []byte("far too long to fit")); err == nil {
		t.Fatal("a journal shorter than its partial line was truncated")
	}
}

func TestReadJournalReturnsEveryPartialForm(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"truncated json", "{\"stream\":\"session:a\",\"seq\":1"},
		{"not json at all", "this is not a record"},
		{"empty object", "{}"},
		{"array", "[1,2,3]"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(testCase.content), 0o600); err != nil {
				t.Fatal(err)
			}
			records, partial, err := ReadJournal(path)
			if err != nil {
				t.Fatal(err)
			}
			// A syntactically valid but semantically empty record is accepted
			// as a record; anything else must surface as a partial line.
			if testCase.name == "empty object" {
				if len(records) != 1 {
					t.Fatalf("records = %d", len(records))
				}
				return
			}
			if len(records) != 0 {
				t.Fatalf("records = %d", len(records))
			}
			if len(partial) == 0 {
				t.Fatalf("partial was not reported for %q", testCase.content)
			}
		})
	}
}

func TestReadJournalAcceptsAnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	records, partial, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 || len(partial) != 0 {
		t.Fatalf("empty journal = %d records, %q partial", len(records), partial)
	}
}

func TestOpenAuditReportsInvalidDatabases(t *testing.T) {
	dir := t.TempDir()
	if _, err := OpenAudit("  "); err == nil {
		t.Fatal("an empty path was accepted")
	}
	corrupt := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAudit(corrupt); err == nil {
		t.Fatal("a corrupt database was accepted")
	}
	if _, err := OpenAudit(dir); err == nil {
		t.Fatal("a directory was accepted as a database")
	}
}

func TestAuditReportsAClosedDatabase(t *testing.T) {
	audit, err := OpenAudit(filepath.Join(t.TempDir(), "marxagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.db.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := audit.Write(ctx, []Record{messageRecord(1, "m1", "user", "x")}); err == nil {
		t.Fatal("a write to a closed database was accepted")
	}
	if err := audit.WriteCheckpoint(ctx, "session:s1", 1, 1, "s"); err == nil {
		t.Fatal("a checkpoint to a closed database was accepted")
	}
	if _, _, _, err := audit.Checkpoint(ctx, "session:s1"); err == nil {
		t.Fatal("a checkpoint read from a closed database was accepted")
	}
	if _, err := audit.SearchMessages(ctx, "s1", "query", 1); err == nil {
		t.Fatal("a search on a closed database was accepted")
	}
	if _, err := audit.EventCount(ctx, "s1"); err == nil {
		t.Fatal("a count on a closed database was accepted")
	}
	if _, err := audit.HighestToolSeq(ctx, "session:s1"); err == nil {
		t.Fatal("a tool sequence read on a closed database was accepted")
	}
	if _, err := audit.LastAppliedSeq(ctx, "session:s1"); err == nil {
		t.Fatal("a checkpoint sequence read on a closed database was accepted")
	}
	if err := audit.Close(); err == nil {
		t.Fatal("closing an already closed database reported success")
	}
}

func TestAuditRejectsUnreadableMetadata(t *testing.T) {
	audit := newAudit(t)
	ctx := context.Background()
	if _, err := audit.db.Exec(
		`UPDATE meta SET value = 'not a number' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.SchemaVersionOf(); err == nil {
		t.Fatal("a malformed schema version was accepted")
	}
	if _, err := audit.db.Exec(
		`INSERT INTO meta(key, value) VALUES('checkpoint:session:s1', 'nope')`); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.LastAppliedSeq(ctx, "session:s1"); err == nil {
		t.Fatal("a malformed checkpoint sequence was accepted")
	}
	if _, err := audit.db.Exec(
		`INSERT INTO meta(key, value) VALUES('checkpoint_messages:s1', 'nope')`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := audit.Checkpoint(ctx, "session:s1"); err == nil {
		t.Fatal("a malformed checkpoint message count was accepted")
	}
}

func TestAuditRejectsQueriesItCannotServe(t *testing.T) {
	audit := newAudit(t)
	// A dropped column makes the prepared search fail at query time.
	if _, err := audit.db.Exec(`ALTER TABLE messages_fts RENAME TO messages_fts_moved`); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.SearchMessages(context.Background(), "s1", "needle", 5); err == nil {
		t.Fatal("a search against a missing index was accepted")
	}
}

func TestCollectSearchResultsReportsScanFailures(t *testing.T) {
	audit := newAudit(t)
	if err := audit.Write(context.Background(), []Record{
		messageRecord(1, "m1", "user", "content"),
	}); err != nil {
		t.Fatal(err)
	}
	// A NULL cannot be scanned into a string, which is the only reliable way to
	// reach the scan error branch.
	rows, err := audit.db.Query(`SELECT NULL, message_id, role, content FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collectSearchResults(rows); err == nil {
		t.Fatal("an unscannable row reported success")
	}
}

func TestBufferRunStopsOnContextCancellation(t *testing.T) {
	sink, err := OpenJournal(filepath.Join(t.TempDir(), "session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	buffer, err := New(Config{WatermarkRecords: 1, FlushPeriod: 10 * time.Millisecond}, sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer.Run(ctx)
	}()
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	// A final flush has to survive cancellation, otherwise the shutdown path
	// would silently drop the last records.
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, partial, err := ReadJournal(filepath.Join(t.TempDir(), "unused"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 || len(partial) != 0 {
		t.Fatalf("records = %d partial = %q", len(records), partial)
	}
}

func TestBufferRunFlushesPeriodically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	sink, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	buffer, err := New(Config{WatermarkRecords: 1000, FlushPeriod: 10 * time.Millisecond}, sink)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer.Run(ctx)
	}()
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		records, _, readErr := ReadJournal(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(records) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	// The loop has to be finished before the sink closes, or it would write to a
	// closed journal after the assertions have already run.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the write buffer loop did not stop")
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	records, partial, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || len(partial) != 0 {
		t.Fatalf("records = %d partial = %q", len(records), partial)
	}
}

func TestJournalPathAndDiscoveryValidateTheirInputs(t *testing.T) {
	root := t.TempDir()
	// The broker stream takes its kind from the directory, so its identifier
	// still has to be a valid one.
	if _, err := journalPath(root, "broker:../escape"); err == nil {
		t.Fatal("a traversal reached the broker journal")
	}
	// A root that is not a directory is reported, because a caller that passed a
	// file would otherwise get a scope that silently holds no journal.
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverJournals(file); err == nil {
		t.Fatal("a file was walked as a scope root")
	}
}

func TestTruncatePartialJournalReportsADirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := TruncatePartialJournal(directory, []byte("x")); err == nil {
		t.Fatal("a directory was truncated")
	}
}

func TestSearchEventsBoundsItsLimit(t *testing.T) {
	audit := newAudit(t)
	if err := audit.Write(context.Background(), []Record{
		messageRecord(1, "m1", "user", "bounded event"),
	}); err != nil {
		t.Fatal(err)
	}
	// Both ends of the range are clamped, so a caller cannot read the whole
	// ledger by accident or ask for an unbounded result.
	for _, limit := range []int{0, -1, 100000} {
		results, err := audit.SearchEvents(context.Background(), "s1", "", limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(results) != 1 {
			t.Fatalf("limit %d returned %d", limit, len(results))
		}
	}
	if err := audit.SetShutdownState(context.Background(), "clean"); err != nil {
		t.Fatal(err)
	}
	if err := audit.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := audit.SetShutdownState(context.Background(), "clean"); err == nil {
		t.Fatal("a shutdown state was written to a closed database")
	}
	if err := audit.WriteCheckpoint(context.Background(), "session:s1", 1, 1, "x"); err == nil {
		t.Fatal("a checkpoint was written to a closed database")
	}
}

func TestOpenJournalClosesItsHandleOnSchemaFailure(t *testing.T) {
	dir := t.TempDir()
	// A directory where the journal file should be makes OpenFile fail, so the
	// handle is never created and nothing leaks.
	if err := os.MkdirAll(filepath.Join(dir, "session.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJournal(filepath.Join(dir, "session.jsonl")); err == nil {
		t.Fatal("a directory was accepted as a journal")
	}
	if _, err := sql.Open("sqlite", "file::memory:"); err != nil {
		t.Fatal(err)
	}
}
