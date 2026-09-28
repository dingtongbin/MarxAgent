// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingSink keeps what it was given, so a test can say which records reached
// which journal rather than only that nothing failed.
type recordingSink struct {
	mu      sync.Mutex
	batches [][]Record
	synced  int
	err     error
}

func (s *recordingSink) Write(_ context.Context, batch []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	copied := make([]Record, len(batch))
	copy(copied, batch)
	s.batches = append(s.batches, copied)
	return nil
}

func (s *recordingSink) Sync(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.synced++
	return nil
}

func (s *recordingSink) records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Record
	for _, batch := range s.batches {
		all = append(all, batch...)
	}
	return all
}

func TestEachStreamReachesItsOwnJournalAndAllOfThemReachTheLedger(t *testing.T) {
	sessionJournal := &recordingSink{}
	eventsJournal := &recordingSink{}
	ledger := &recordingSink{}
	sink, err := NewSplitSink(map[string]Sink{
		StreamSession: sessionJournal,
		StreamEvents:  eventsJournal,
	}, ledger, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	batch := []Record{
		{Stream: "session:s1", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{"n":1}`)},
		{Stream: "events:s1", Seq: 1, Type: RecordEvent, Data: json.RawMessage(`{"n":2}`)},
		{Stream: "session:s1", Seq: 2, Type: RecordMessage, Data: json.RawMessage(`{"n":3}`)},
	}
	if err := sink.Write(context.Background(), batch); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A message history that also held the raw events would be a file where each
	// event appears twice and neither is readable on its own.
	if got := len(sessionJournal.records()); got != 2 {
		t.Fatalf("the message journal holds %d records, want the 2 messages", got)
	}
	if got := len(eventsJournal.records()); got != 1 {
		t.Fatalf("the event journal holds %d records, want the 1 event", got)
	}
	// The ledger is the audited copy of everything, and a record missing from it
	// because of which stream it belonged to would be a record the audit cannot
	// show.
	if got := len(ledger.records()); got != 3 {
		t.Fatalf("the ledger holds %d records, want all 3", got)
	}
	if err := sink.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if sessionJournal.synced != 1 || eventsJournal.synced != 1 || ledger.synced != 1 {
		t.Fatalf("syncs = %d/%d/%d, want one each", sessionJournal.synced, eventsJournal.synced, ledger.synced)
	}
}

func TestARecordForAStreamWithNoJournalBehindItIsCounted(t *testing.T) {
	sessionJournal := &recordingSink{}
	sink, err := NewSplitSink(map[string]Sink{StreamSession: sessionJournal}, nil, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := sink.Write(context.Background(), []Record{
		{Stream: "session:s1", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{}`)},
		{Stream: "subagent:s1", Seq: 1, Type: RecordBrokerMessage, Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := len(sessionJournal.records()); got != 1 {
		t.Fatalf("the message journal holds %d records, want 1", got)
	}
	// A record written nowhere is a number rather than a silence, because silence
	// is what a durability bug looks like from the outside.
	if got := sink.Unrouted(); got != 1 {
		t.Fatalf("unrouted = %d, want the one record with no journal", got)
	}
}

func TestOneJournalFailingIsReportedRatherThanSwallowed(t *testing.T) {
	failing := &recordingSink{err: errors.New("the disk said no")}
	sink, err := NewSplitSink(map[string]Sink{StreamSession: failing}, &recordingSink{}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := sink.Write(context.Background(), []Record{
		{Stream: "session:s1", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{}`)},
	}); err == nil {
		t.Fatal("a journal that failed to write was reported as a success")
	}
}

func TestARouteWithNoSinkIsRefused(t *testing.T) {
	if _, err := NewSplitSink(map[string]Sink{StreamSession: nil}, nil, nil); err == nil {
		t.Fatal("a route with no journal behind it was accepted")
	}
	if _, err := NewSplitSink(map[string]Sink{"": &recordingSink{}}, nil, nil); err == nil {
		t.Fatal("a route with no stream kind was accepted")
	}
}

func TestTheLedgerCanBeGivenADifferentShapeThanTheJournals(t *testing.T) {
	// The journal is read back to rebuild a conversation and the ledger is
	// searched, so the two cannot hold the same payload. The copy the ledger gets
	// is derived here, and a record that cannot be turned into it is a failure
	// rather than a message quietly left out of the audited record.
	sessionJournal := &recordingSink{}
	ledger := &recordingSink{}
	indexable := func(record Record) (Record, error) {
		if record.Type != RecordMessage {
			return record, nil
		}
		record.Data = json.RawMessage(`{"id":"m1","role":"user","content":"searchable"}`)
		return record, nil
	}
	sink, err := NewSplitSink(map[string]Sink{StreamSession: sessionJournal}, ledger, indexable)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := sink.Write(context.Background(), []Record{
		{Stream: "session:s1", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{"content":[{"type":"text","text":"hi"}]}`)},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	journal := sessionJournal.records()
	if len(journal) != 1 || !strings.Contains(string(journal[0].Data), `"content":[{"type":"text"`) {
		t.Fatalf("the journal was given the ledger's copy: %s", journal[0].Data)
	}
	audited := ledger.records()
	if len(audited) != 1 || string(audited[0].Data) != `{"id":"m1","role":"user","content":"searchable"}` {
		t.Fatalf("the ledger was given the journal's copy: %s", audited[0].Data)
	}
}

func TestARecordTheLedgerCannotTakeIsAFailure(t *testing.T) {
	sink, err := NewSplitSink(map[string]Sink{StreamSession: &recordingSink{}}, &recordingSink{},
		func(Record) (Record, error) { return Record{}, errors.New("no shape for it") })
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := sink.Write(context.Background(), []Record{
		{Stream: "session:s1", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{}`)},
	}); err == nil {
		t.Fatal("a record the ledger could not take was reported as written")
	}
}

func TestStreamKindIsWhatSaysWhichJournalARecordBelongsIn(t *testing.T) {
	cases := map[string]string{
		"session:abc":           StreamSession,
		"events:abc":            StreamEvents,
		"session:with:colons":   StreamSession,
		"broker":                "broker",
		"":                      "",
		"subagent:agent-1":      StreamSubAgent,
		"session:20260102-1504": StreamSession,
	}
	for stream, want := range cases {
		if got := StreamKind(stream); got != want {
			t.Fatalf("StreamKind(%q) = %q, want %q", stream, got, want)
		}
	}
}

func TestAConversationWrittenThroughTheSplitSinkCanBeReadBackAsAPlainJournal(t *testing.T) {
	// The point of splitting at all is that the files stay readable on their own,
	// so this reads them the way a person would: one JSON object per line.
	directory := t.TempDir()
	sessionJournal, err := OpenJournal(filepath.Join(directory, "session.jsonl"))
	if err != nil {
		t.Fatalf("open the message journal: %v", err)
	}
	eventsJournal, err := OpenJournal(filepath.Join(directory, "events.jsonl"))
	if err != nil {
		t.Fatalf("open the event journal: %v", err)
	}
	sink, err := NewSplitSink(map[string]Sink{
		StreamSession: sessionJournal,
		StreamEvents:  eventsJournal,
	}, nil, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	buffer, err := New(Config{}, sink)
	if err != nil {
		t.Fatalf("buffer: %v", err)
	}
	for index, record := range []Record{
		{Stream: "session:s1", Type: RecordMessage, Data: json.RawMessage(`{"role":"user"}`)},
		{Stream: "events:s1", Type: RecordEvent, Data: json.RawMessage(`{"kind":"state_change"}`)},
		{Stream: "session:s1", Type: RecordMessage, Data: json.RawMessage(`{"role":"assistant"}`)},
	} {
		if err := buffer.Append(record); err != nil {
			t.Fatalf("append %d: %v", index, err)
		}
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	messages, partial, err := ReadJournal(filepath.Join(directory, "session.jsonl"))
	if err != nil {
		t.Fatalf("read the message journal: %v", err)
	}
	if len(partial) != 0 || len(messages) != 2 {
		t.Fatalf("message journal = %d records and %d partial bytes", len(messages), len(partial))
	}
	events, partial, err := ReadJournal(filepath.Join(directory, "events.jsonl"))
	if err != nil {
		t.Fatalf("read the event journal: %v", err)
	}
	if len(partial) != 0 || len(events) != 1 {
		t.Fatalf("event journal = %d records and %d partial bytes", len(events), len(partial))
	}
	if err := sessionJournal.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := eventsJournal.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Both files are closed, so the directory can be removed, which on Windows is
	// the difference between a test that cleans up and one that leaves a lock
	// behind for the next run to trip over.
	if err := os.RemoveAll(directory); err != nil {
		t.Fatalf("remove: %v", err)
	}
}
