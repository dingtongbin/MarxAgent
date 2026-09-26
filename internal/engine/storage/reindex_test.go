// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestReindexRebuildsTheLedgerFromTheJournals covers the disaster recovery path:
// the ledger is destroyed and the journals alone bring it back.
func TestReindexReportsFailures(t *testing.T) {
	ctx := context.Background()
	audit := newAudit(t)
	// A closed ledger reports the rebuild instead of silently doing nothing.
	if err := audit.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Reindex(ctx, t.TempDir(), audit); err == nil {
		t.Fatal("a rebuild ran against a closed ledger")
	}
	if err := audit.CheckRebuildable(ctx); err == nil {
		t.Fatal("a closed ledger reported itself rebuildable")
	}
	if _, err := audit.Verify(ctx, t.TempDir()); err == nil {
		t.Fatal("a closed ledger verified")
	}
}

func TestReindexReportsAnUnreadableJournal(t *testing.T) {
	ctx := context.Background()
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	// A directory where a journal belongs cannot be parsed, and the rebuild has
	// to say so rather than report a clean run over fewer records.
	directory := filepath.Join(scope.SessionsDir(), "s1", "session.jsonl")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Reindex(ctx, scope.DataRoot(), scope.Audit()); err == nil {
		t.Fatal("an unreadable journal was skipped silently")
	}
	if _, err := scope.Audit().Verify(ctx, scope.DataRoot()); err == nil {
		t.Fatal("an unreadable journal verified")
	}
}

func TestReindexRebuildOnlyRebuilds(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	scope, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	// The ledger holds something the journals do not, which is what makes a
	// replace rather than a merge the honest choice.
	if err := scope.Audit().Write(ctx, []Record{messageRecord(1, "ghost", "user", "only in the ledger")}); err != nil {
		t.Fatal(err)
	}
	writeScopeJournal(t, scope.DataRoot(), "session:s1", []Record{
		messageRecord(1, "m1", "user", "rebuild me"),
	})
	if _, err := Reindex(ctx, scope.DataRoot(), scope.Audit()); err != nil {
		t.Fatal(err)
	}
	ghost, err := scope.Audit().SearchMessages(ctx, "s1", "only in the ledger", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ghost) != 0 {
		t.Fatalf("a rebuild merged instead of replacing: %#v", ghost)
	}
	kept, err := scope.Audit().SearchMessages(ctx, "s1", "rebuild me", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Fatalf("kept = %#v", kept)
	}
}

func TestReindexRebuildsTheLedgerFromTheJournals(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	scope, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	// Write two sessions worth of history through the normal path.
	for _, sessionID := range []string{"s1", "s2"} {
		journal := writeScopeJournal(t, scope.DataRoot(), StreamSession+":"+sessionID, []Record{
			messageRecord(1, "m1", "user", "rebuildable needle one"),
			messageRecord(2, "m2", "assistant", "rebuildable needle two"),
		})
		if journal == "" {
			t.Fatal("the journal was not written")
		}
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	// Destroy the ledger the way corruption would: the journals are untouched.
	database := filepath.Join(root, ProjectDirName, DBFileName)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(database + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	rebuilt, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if err := rebuilt.Audit().CheckRebuildable(ctx); err != nil {
		t.Fatal(err)
	}
	// Before the rebuild the ledger knows nothing.
	results, err := rebuilt.Audit().SearchMessages(ctx, "s1", "rebuildable", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("a fresh ledger already had rows: %#v", results)
	}
	report, err := Reindex(ctx, rebuilt.DataRoot(), rebuilt.Audit())
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 4 || report.Indexed != 4 || report.Skipped != 0 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Streams) != 2 || report.Streams[0] != "session:s1" {
		t.Fatalf("streams = %v", report.Streams)
	}
	results, err = rebuilt.Audit().SearchMessages(ctx, "s1", "rebuildable", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("rebuilt messages = %d", len(results))
	}
	// Running it again must not double anything, because a user who is unsure
	// will run it twice.
	again, err := Reindex(ctx, rebuilt.DataRoot(), rebuilt.Audit())
	if err != nil {
		t.Fatal(err)
	}
	if again.Indexed != 4 {
		t.Fatalf("second rebuild = %#v", again)
	}
	results, err = rebuilt.Audit().SearchMessages(ctx, "s1", "rebuildable", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("a second rebuild duplicated rows: %d", len(results))
	}
}

// TestReindexDropsUnusableRecords proves the rebuild does not import junk.
func TestReindexDropsUnusableRecords(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	scope, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	writeScopeJournal(t, scope.DataRoot(), "session:s1", []Record{
		{Stream: "", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{}`)},
		{Stream: "session:s1", Seq: -5, Type: RecordMessage, Data: json.RawMessage(`{}`)},
		{Stream: "session:s1", Seq: 2, Type: "", Data: json.RawMessage(`{}`)},
		messageRecord(3, "m3", "user", "the only usable one"),
	})
	report, err := Reindex(ctx, scope.DataRoot(), scope.Audit())
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 3 || report.Indexed != 1 || report.Records != 4 {
		t.Fatalf("report = %#v", report)
	}
	results, err := scope.Audit().SearchMessages(ctx, "s1", "usable", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
}

// TestReindexRepairsAHalfWrittenTail covers the case a rebuild is most likely to
// meet in practice: the process died mid write.
func TestReindexRepairsAHalfWrittenTail(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	scope, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	path := writeScopeJournal(t, scope.DataRoot(), "session:s1", []Record{
		messageRecord(1, "m1", "user", "intact needle"),
	})
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"stream":"session:s1","seq":2,"typ`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Reindex(ctx, scope.DataRoot(), scope.Audit())
	if err != nil {
		t.Fatal(err)
	}
	if report.TruncatedBytes == 0 {
		t.Fatalf("the half written line was not repaired: %#v", report)
	}
	if report.Indexed != 1 {
		t.Fatalf("report = %#v", report)
	}
	records, partial, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 0 || len(records) != 1 {
		t.Fatalf("records = %d partial = %q", len(records), partial)
	}
}

// TestReindexClearsStaleCheckpoints matters because a checkpoint left over from
// before the rebuild would make the next recovery skip records that are now in
// the ledger under a fresh identity.
func TestReindexClearsStaleCheckpoints(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	scope, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	writeScopeJournal(t, scope.DataRoot(), "session:s1", []Record{
		messageRecord(1, "m1", "user", "checkpointed needle"),
	})
	if err := scope.Checkpoint(ctx, "s1", 99, 99, "stale"); err != nil {
		t.Fatal(err)
	}
	if err := scope.CheckpointStream(ctx, "events:s1", 42); err != nil {
		t.Fatal(err)
	}
	if _, err := Reindex(ctx, scope.DataRoot(), scope.Audit()); err != nil {
		t.Fatal(err)
	}
	if seq, _, _, err := scope.Audit().Checkpoint(ctx, "session:s1"); err != nil || seq != 0 {
		t.Fatalf("a stale checkpoint survived: %d %v", seq, err)
	}
	if seq, err := scope.Audit().LastAppliedSeq(ctx, "events:s1"); err != nil || seq != 0 {
		t.Fatalf("a stale stream checkpoint survived: %d %v", seq, err)
	}
}

func TestVerifyComparesJournalsAndLedger(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	scope, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	writeScopeJournal(t, scope.DataRoot(), "session:s1", []Record{
		messageRecord(1, "m1", "user", "verify needle"),
		messageRecord(2, "m2", "user", "verify needle two"),
	})
	// The ledger is empty until something writes to it.
	report, err := scope.Audit().Verify(ctx, scope.DataRoot())
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 2 || report.Indexed != 0 {
		t.Fatalf("report = %#v", report)
	}
	if _, err := Reindex(ctx, scope.DataRoot(), scope.Audit()); err != nil {
		t.Fatal(err)
	}
	report, err = scope.Audit().Verify(ctx, scope.DataRoot())
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 2 || report.Indexed != 2 {
		t.Fatalf("report after rebuild = %#v", report)
	}
	if len(report.Streams) != 1 || report.Streams[0] != "session:s1" {
		t.Fatalf("streams = %v", report.Streams)
	}
}

func TestReindexValidatesItsInputs(t *testing.T) {
	ctx := context.Background()
	if _, err := Reindex(ctx, t.TempDir(), nil); err == nil {
		t.Fatal("a rebuild without a ledger was accepted")
	}
	if _, err := Reindex(ctx, "", nil); err == nil {
		t.Fatal("an empty root was accepted")
	}
	audit := newAudit(t)
	if _, err := Reindex(ctx, filepath.Join(t.TempDir(), "absent"), audit); err == nil {
		t.Fatal("a missing root was accepted")
	}
	if _, err := audit.Verify(ctx, filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing root was verified")
	}
}

func TestReindexRefusesADriftedLedger(t *testing.T) {
	audit := newAudit(t)
	ctx := context.Background()
	if err := audit.CheckRebuildable(ctx); err != nil {
		t.Fatal(err)
	}
	// A ledger from another schema must not be rebuilt into, because the
	// journals would be read into a shape the code no longer understands.
	if _, err := audit.db.Exec(`UPDATE meta SET value = '999' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := audit.CheckRebuildable(ctx); err == nil {
		t.Fatal("a drifted ledger was accepted")
	}
	if _, err := Reindex(ctx, t.TempDir(), audit); err != nil {
		t.Fatal(err)
	}
}
