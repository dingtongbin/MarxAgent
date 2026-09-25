// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanMarkerNeedsBothHalves(t *testing.T) {
	root := t.TempDir()
	audit, err := OpenAudit(filepath.Join(root, "marxagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()
	marker, err := NewCleanMarker(root, audit)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A fresh scope has never shut down, so it is not clean.
	clean, err := marker.IsClean(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Fatal("a fresh scope reported a clean shutdown")
	}
	if err := marker.MarkClean(ctx); err != nil {
		t.Fatal(err)
	}
	if clean, err = marker.IsClean(ctx); err != nil || !clean {
		t.Fatalf("clean = %v err = %v", clean, err)
	}
	// Removing only the sentinel must be enough to distrust the shutdown, which
	// is the whole point of double writing the marker.
	if err := os.Remove(filepath.Join(root, CleanSentinelName)); err != nil {
		t.Fatal(err)
	}
	if clean, err = marker.IsClean(ctx); err != nil || clean {
		t.Fatalf("a missing sentinel still read as clean: %v %v", clean, err)
	}
	if err := marker.MarkDirty(ctx); err != nil {
		t.Fatal(err)
	}
	if clean, err = marker.IsClean(ctx); err != nil || clean {
		t.Fatalf("dirty = %v err = %v", clean, err)
	}
}

func TestCleanMarkerTreatsACorruptSentinelAsDirty(t *testing.T) {
	root := t.TempDir()
	marker, err := NewCleanMarker(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, CleanSentinelName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	clean, err := marker.IsClean(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Fatal("a corrupt sentinel read as clean")
	}
	if err := marker.MarkClean(context.Background()); err != nil {
		t.Fatal(err)
	}
	clean, err = marker.IsClean(context.Background())
	if err != nil || !clean {
		t.Fatalf("clean = %v err = %v", clean, err)
	}
	if err := marker.MarkDirty(context.Background()); err != nil {
		t.Fatal(err)
	}
	clean, err = marker.IsClean(context.Background())
	if err != nil || clean {
		t.Fatalf("dirty = %v err = %v", clean, err)
	}
	if err := marker.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := marker.Remove(); err != nil {
		t.Fatalf("a second remove failed: %v", err)
	}
}

func TestNewCleanMarkerRejectsAnEmptyRoot(t *testing.T) {
	if _, err := NewCleanMarker("  ", nil); err == nil {
		t.Fatal("an empty root was accepted")
	}
}

func TestSetShutdownStateRejectsUnknownStates(t *testing.T) {
	audit := newAudit(t)
	if err := audit.SetShutdownState(context.Background(), "maybe"); err == nil {
		t.Fatal("an unknown shutdown state was accepted")
	}
}

func newRecoveryScope(t *testing.T) (string, *AuditSink) {
	t.Helper()
	root := t.TempDir()
	audit, err := OpenAudit(filepath.Join(root, "marxagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	return root, audit
}

func TestRecoverReplaysAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	root, audit := newRecoveryScope(t)
	journal := filepath.Join(root, "session-s1.jsonl")
	records := []Record{
		messageRecord(1, "m1", "user", "replayed needle"),
		messageRecord(2, "m2", "assistant", "an answer"),
	}
	writeJournalRecords(t, journal, records)

	// The journal ran ahead of the ledger, which is the normal journal first
	// ordering, so recovery has to catch the ledger up.
	if err := audit.WriteCheckpoint(ctx, "s1", 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	result, err := Recover(ctx, root, audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
	session := result.Sessions[0]
	if session.Stream != "session:s1" || session.Records != 2 || session.Replayed != 2 {
		t.Fatalf("session = %#v", session)
	}
	if session.LedgerAhead {
		t.Fatalf("the ledger was not behind: %#v", session)
	}
	results, err := audit.SearchMessages(ctx, "s1", "replayed", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MessageID != "m1" {
		t.Fatalf("replayed search = %#v", results)
	}

	// A second run must converge rather than duplicate, which is what makes it
	// safe to run on every start that did not shut down cleanly.
	if err := audit.WriteCheckpoint(ctx, "s1", 2, 2, ""); err != nil {
		t.Fatal(err)
	}
	second, err := Recover(ctx, root, audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Sessions) != 1 {
		t.Fatalf("second sessions = %#v", second.Sessions)
	}
	if second.Sessions[0].Replayed != 0 || second.Sessions[0].SkippedReplay != 2 {
		t.Fatalf("second run was not idempotent: %#v", second.Sessions[0])
	}
	again, err := audit.SearchMessages(ctx, "s1", "replayed", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("a replay duplicated rows: %d", len(again))
	}
}

func TestRecoverTruncatesAHalfWrittenLine(t *testing.T) {
	ctx := context.Background()
	root, audit := newRecoveryScope(t)
	journal := filepath.Join(root, "session-s1.jsonl")
	writeJournalRecords(t, journal, []Record{messageRecord(1, "m1", "user", "first needle")})
	file, err := os.OpenFile(journal, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what an abrupt kill leaves: a record cut in half.
	if _, err := file.WriteString(`{"stream":"session:s1","seq":2,"type":"message","data":{"id":"m2",`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := audit.WriteCheckpoint(ctx, "s1", 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	result, err := Recover(ctx, root, audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
	session := result.Sessions[0]
	if session.TruncatedBytes == 0 {
		t.Fatalf("the half written line was not reported: %#v", session)
	}
	if session.Records != 1 || session.SkippedReplay != 1 {
		t.Fatalf("session = %#v", session)
	}
	// The journal must now be back on a record boundary, so the next append
	// cannot produce a corrupt line of its own.
	parsed, partial, err := ReadJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 0 || len(parsed) != 1 {
		t.Fatalf("after recovery records = %d partial = %q", len(parsed), partial)
	}
	sink, err := OpenJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(ctx, []Record{messageRecord(2, "m2", "user", "second")}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	parsed, partial, err = ReadJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 || len(partial) != 0 {
		t.Fatalf("after append records = %d partial = %q", len(parsed), partial)
	}
}

func TestRecoverReportsALedgerAheadOfTheJournal(t *testing.T) {
	ctx := context.Background()
	root, audit := newRecoveryScope(t)
	writeJournalRecords(t, filepath.Join(root, "session-s1.jsonl"), []Record{
		messageRecord(1, "m1", "user", "one"),
	})
	// The checkpoint claims far more than the journal holds, which means the
	// journal lost data rather than merely stopping early.
	if err := audit.WriteCheckpoint(ctx, "s1", 99, 99, ""); err != nil {
		t.Fatal(err)
	}
	result, err := Recover(ctx, root, audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 || !result.Sessions[0].LedgerAhead {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
	if len(result.Sessions[0].Warnings) == 0 {
		t.Fatal("a ledger ahead condition produced no warning")
	}
	if result.Actions == 0 {
		t.Fatal("the recovery run left no audit trail")
	}
	// The warning itself has to be queryable from the audit ledger, which is
	// the audit self bootstrapping requirement.
	found, err := audit.SearchEvents(ctx, "startup", "ledger", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("the recovery action was not recorded in the ledger")
	}
	for _, event := range found {
		if event.EventType != RecordTypeRecoveryAction {
			t.Fatalf("event = %#v", event)
		}
	}
}

func TestRecoverFilesBrokerMessagesAsOrphans(t *testing.T) {
	ctx := context.Background()
	root, audit := newRecoveryScope(t)
	writeJournalRecords(t, filepath.Join(root, "subagent-a1.jsonl"), []Record{
		{Stream: "subagent:a1", Seq: 1, Type: RecordBrokerMessage,
			Data: json.RawMessage(`{"id":"m1","to":"a2","content":"take this"}`)},
		{Stream: "subagent:a1", Seq: 2, Type: RecordBrokerMessage,
			Data: json.RawMessage(`{"id":"m2","to":"broadcast","content":"anyone"}`)},
	})
	if err := audit.WriteCheckpoint(ctx, "a1", 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	result, err := Recover(ctx, root, audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
	if result.Sessions[0].Orphaned != 2 {
		t.Fatalf("orphaned = %d", result.Sessions[0].Orphaned)
	}
	if result.Actions == 0 {
		t.Fatal("orphan filing left no audit trail")
	}
}

func TestRecoverSkipsUnusableRecords(t *testing.T) {
	ctx := context.Background()
	root, audit := newRecoveryScope(t)
	writeJournalRecords(t, filepath.Join(root, "session-s1.jsonl"), []Record{
		{Stream: "", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{}`)},
		{Stream: "session:s1", Seq: 0, Type: "", Data: json.RawMessage(`{}`)},
	})
	result, err := Recover(ctx, root, audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
	if result.Sessions[0].Skipped != 2 {
		t.Fatalf("skipped = %d", result.Sessions[0].Skipped)
	}
	if len(result.Sessions[0].Warnings) == 0 {
		t.Fatal("a skipped record produced no warning")
	}
}

func TestRecoverIgnoresUnrelatedFiles(t *testing.T) {
	ctx := context.Background()
	root, audit := newRecoveryScope(t)
	for _, name := range []string{"notes.txt", "index.json", "no-separator.jsonl", ".clean"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Recover(ctx, root, audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 0 {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
	if err := os.WriteFile(filepath.Join(root, "session-s1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = Recover(ctx, root, audit)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 1 {
		t.Fatalf("sessions after a valid file = %#v", result.Sessions)
	}
}

func TestRecoverRejectsABadRoot(t *testing.T) {
	if _, err := Recover(context.Background(), "  ", nil); err == nil {
		t.Fatal("an empty root was accepted")
	}
	if _, err := Recover(context.Background(), filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("a missing root was accepted")
	}
}

func TestRecoverRejectsANegativeSequence(t *testing.T) {
	ctx := context.Background()
	root, _ := newRecoveryScope(t)
	writeJournalRecords(t, filepath.Join(root, "session-s1.jsonl"), []Record{
		{Stream: "session:s1", Seq: -1, Type: RecordMessage, Data: json.RawMessage(`{}`)},
	})
	if _, err := Recover(ctx, root, nil); err == nil {
		t.Fatal("a negative sequence was accepted")
	}
}

func TestSearchEventsCoversEveryRecordType(t *testing.T) {
	ctx := context.Background()
	audit := newAudit(t)
	if err := audit.Write(ctx, []Record{
		messageRecord(1, "m1", "user", "shared fragment"),
		{Stream: "session:s1", Seq: 2, Type: RecordEvent,
			Data: json.RawMessage(`{"type":"state_change","shared":true}`)},
		{Stream: "session:other", Seq: 1, Type: RecordEvent,
			Data: json.RawMessage(`{"type":"other"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	// Every record reaches the ledger, so the event search covers message
	// payloads as well, which is what lets an auditor look at one stream in one
	// place instead of joining two tables by hand.
	scoped, err := audit.SearchEvents(ctx, "s1", "shared", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 {
		t.Fatalf("scoped = %#v", scoped)
	}
	types := map[string]bool{}
	for _, event := range scoped {
		if event.SessionID != "s1" || event.TS.IsZero() {
			t.Fatalf("event = %#v", event)
		}
		types[event.EventType] = true
	}
	if !types[RecordMessage] || !types[RecordEvent] {
		t.Fatalf("types = %#v", types)
	}
	unscoped, err := audit.SearchEvents(ctx, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(unscoped) != 3 {
		t.Fatalf("unscoped = %d", len(unscoped))
	}
	// A limit of zero falls back to the default rather than returning nothing,
	// so a caller cannot accidentally read the whole ledger.
	limited, err := audit.SearchEvents(ctx, "s1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("limited = %d", len(limited))
	}
	none, err := audit.SearchEvents(ctx, "s1", "absent substring", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("none = %#v", none)
	}
}

func TestIncompleteMessageMarksPartialStreams(t *testing.T) {
	record := Record{
		Stream: "session:s1",
		Seq:    3,
		Type:   RecordPartialStream,
		Data:   json.RawMessage(`{"text":"half a th`),
		Ts:     time.Unix(3, 0).UTC(),
	}
	payload, ok := IncompleteMessage(record)
	if !ok {
		t.Fatal("a partial stream was not converted")
	}
	var message struct {
		ID         string `json:"id"`
		Role       string `json:"role"`
		Content    string `json:"content"`
		Incomplete bool   `json:"incomplete"`
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	if !message.Incomplete {
		t.Fatal("the interrupted marker was dropped")
	}
	if message.Role != "assistant" || message.Content != "half a th" {
		t.Fatalf("message = %#v", message)
	}
	if message.ID != "partial-3" {
		t.Fatalf("id = %q", message.ID)
	}

	// A partial that carried a whole message keeps its fields and still gains
	// the marker, so the model never sees it as complete.
	record.Data = json.RawMessage(`{"message":{"id":"m9","role":"assistant","content":"thinking out loud","complete":true}}`)
	payload, ok = IncompleteMessage(record)
	if !ok {
		t.Fatal("a partial stream was not converted")
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	if message.ID != "m9" || message.Content != "thinking out loud" || !message.Incomplete {
		t.Fatalf("message = %#v", message)
	}

	record.Data = json.RawMessage(`{"thinking":"pondering"}`)
	payload, ok = IncompleteMessage(record)
	if !ok {
		t.Fatal("a thinking partial was not converted")
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	if message.Content != "pondering" {
		t.Fatalf("message = %#v", message)
	}

	// A record that is not a partial stream is not converted.
	if _, ok := IncompleteMessage(messageRecord(1, "m1", "user", "x")); ok {
		t.Fatal("a message record was converted")
	}
}
