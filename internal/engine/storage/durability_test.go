// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScopeSurvivesAnAbruptShutdown walks the whole durability contract in one
// flow, because the promise is about the sequence rather than any single part:
// a record written before a kill must still be readable after recovery, the
// repair must be idempotent, and the broker must not resurrect dead work.
func TestScopeSurvivesAnAbruptShutdown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	scope, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := scope.Marker()
	if err != nil {
		t.Fatal(err)
	}
	// A running instance records that it has not finished shutting down.
	if err := marker.MarkDirty(ctx); err != nil {
		t.Fatal(err)
	}
	sessionJournal, eventsJournal, err := scope.OpenSessionJournals("s1")
	if err != nil {
		t.Fatal(err)
	}
	// The handles stay open on purpose: recovery has to work from what reached
	// the disk, not from anything still in memory. They are closed at the end so
	// the temporary directory can be removed on Windows.
	defer sessionJournal.Close()
	defer eventsJournal.Close()
	// A session has two streams with independent sequences, and each pairs its
	// own journal with the shared ledger, which is the real topology.
	sessionBuffer, err := New(Config{WatermarkRecords: 8, FlushPeriod: time.Hour},
		sessionJournal, scope.Audit())
	if err != nil {
		t.Fatal(err)
	}
	eventsBuffer, err := New(Config{WatermarkRecords: 8, FlushPeriod: time.Hour},
		eventsJournal, scope.Audit())
	if err != nil {
		t.Fatal(err)
	}
	const written = 6
	for index := 1; index <= written; index++ {
		if err := sessionBuffer.Append(Record{
			Stream: StreamSession + ":s1",
			Type:   RecordMessage,
			Data: messagePayload(fmt.Sprintf("m%d", index), "user",
				fmt.Sprintf("crash safe needle %d", index)),
		}); err != nil {
			t.Fatal(err)
		}
		if err := eventsBuffer.Append(Record{
			Stream: StreamEvents + ":s1",
			Type:   RecordEvent,
			Data:   messagePayload(fmt.Sprintf("e%d", index), "user", "state changed"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// The boundary trigger fires, so these records are on disk before the kill.
	if err := sessionBuffer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eventsBuffer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	broker, err := scope.OpenBrokerJournal()
	if err != nil {
		t.Fatal(err)
	}
	brokerMessage := Record{
		Stream: "broker:journal", Seq: 1, Type: RecordBrokerMessage,
		Data: []byte(`{"id":"b1","to":"child","content":"unclaimed work"}`),
	}
	if err := broker.Write(ctx, []Record{brokerMessage}); err != nil {
		t.Fatal(err)
	}
	// A kill reclaims the handle without any further flush, so the journal is
	// closed here only to let the test release it.
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	// A kill leaves the handles to the operating system, which reclaims them, and
	// leaves whatever reached the disk. Releasing the scope here models exactly
	// that: no further flush, no clean marker, but the data already written
	// stays on disk.
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := func() (bool, error) {
		restartMarker, markerErr := reopened.Marker()
		if markerErr != nil {
			return false, markerErr
		}
		return restartMarker.IsClean(ctx)
	}()
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Fatal("a scope abandoned mid shutdown reported a clean shutdown")
	}
	first, err := reopened.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Every record written before the kill has to come back, from both streams,
	// and the broker message has to be filed rather than delivered.
	replayed := map[string]int{}
	orphaned := 0
	for _, session := range first.Sessions {
		replayed[session.Stream] = session.Replayed
		orphaned += session.Orphaned
	}
	if replayed["session:s1"] != written || replayed["events:s1"] != written {
		t.Fatalf("replayed = %#v, want %d from each stream", replayed, written)
	}
	if replayed["broker:journal"] != 1 {
		t.Fatalf("the broker journal was not replayed: %#v", replayed)
	}
	if orphaned != 1 {
		t.Fatalf("orphaned = %d, want the one unclaimed broker message", orphaned)
	}
	results, err := reopened.Audit().SearchMessages(ctx, "s1", "needle", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != written {
		t.Fatalf("recovered messages = %d", len(results))
	}
	// The checkpoints are what make the second pass a no op instead of a second
	// copy of the same history.
	if err := reopened.Checkpoint(ctx, "s1", written, written, "six messages"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CheckpointStream(ctx, "events:s1", written); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CheckpointStream(ctx, "broker:journal", 1); err != nil {
		t.Fatal(err)
	}
	second, err := reopened.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range second.Sessions {
		if session.Replayed != 0 {
			t.Fatalf("recovery was not idempotent: %#v", session)
		}
	}
	again, err := reopened.Audit().SearchMessages(ctx, "s1", "needle", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != written {
		t.Fatalf("a second recovery changed the message count to %d", len(again))
	}
	// An orderly shutdown writes the clean marker, so the next start skips
	// recovery entirely.
	finalMarker, err := reopened.Marker()
	if err != nil {
		t.Fatal(err)
	}
	if err := finalMarker.MarkClean(ctx); err != nil {
		t.Fatal(err)
	}
	// The scope has to be released before it can be opened again, which is the
	// exclusivity the lock exists to provide.
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	thirdMarker, err := third.Marker()
	if err != nil {
		t.Fatal(err)
	}
	isClean, err := thirdMarker.IsClean(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !isClean {
		t.Fatal("an orderly shutdown did not leave a clean marker")
	}
}

func messagePayload(id, role, content string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"role":%q,"content":%q}`, id, role, content))
}

// TestRecoveryTruncatesAJournalThatWasCutMidRecord covers the one case the unit
// tests cannot: a journal whose last line is a real record that was cut in half
// by the kill, sitting next to complete records.
func TestRecoveryTruncatesAJournalThatWasCutMidRecord(t *testing.T) {
	ctx := context.Background()
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	sessionJournal, eventsJournal, err := scope.OpenSessionJournals("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionJournal.Write(ctx, []Record{
		{Stream: "session:s1", Seq: 1, Type: RecordMessage, Data: messagePayload("m1", "user", "intact")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := eventsJournal.Write(ctx, []Record{
		{Stream: "events:s1", Seq: 1, Type: RecordPartialStream,
			Data: []byte(`{"text":"half written"}`), Ts: time.Unix(1, 0).UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	// Cut the events journal mid record, exactly as a power loss would.
	path := filepath.Join(scope.SessionsDir(), "s1", "events.jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"stream":"events:s1","seq":2,"type":"par`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sessionJournal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := eventsJournal.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := scope.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	truncated := 0
	for _, session := range result.Sessions {
		if session.Stream == "events:s1" {
			truncated = session.TruncatedBytes
		}
	}
	if truncated == 0 {
		t.Fatalf("the cut record was not truncated: %#v", result.Sessions)
	}
	parsed, partial, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 0 {
		t.Fatalf("a partial line survived recovery: %q", partial)
	}
	// The intact record is the partial stream, which becomes an interrupted
	// message rather than a complete one.
	if len(parsed) != 1 || parsed[0].Type != RecordPartialStream {
		t.Fatalf("records = %#v", parsed)
	}
	payload, ok := IncompleteMessage(parsed[0])
	if !ok {
		t.Fatal("the interrupted record was not converted")
	}
	if len(payload) == 0 {
		t.Fatal("the interrupted message lost its content")
	}
}
