// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeJournalRecords(t *testing.T, path string, records []Record) {
	t.Helper()
	sink, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	if err := sink.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJournalWritesOneLinePerRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-a.jsonl")
	records := []Record{
		{Stream: "session:a", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{"text":"one"}`), Ts: time.Unix(1, 0).UTC()},
		{Stream: "session:a", Seq: 2, Type: RecordEvent, Data: json.RawMessage(`{"type":"state_change"}`), Ts: time.Unix(2, 0).UTC()},
	}
	writeJournalRecords(t, path, records)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d: %q", len(lines), raw)
	}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line is not JSON: %q", line)
		}
	}
	parsed, partial, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 0 {
		t.Fatalf("partial = %q", partial)
	}
	if len(parsed) != 2 || parsed[1].Seq != 2 || parsed[1].Type != RecordEvent {
		t.Fatalf("records = %#v", parsed)
	}
}

func TestJournalIsUsableAsASink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-a.jsonl")
	sink, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	buffer, err := New(Config{WatermarkRecords: 2, FlushPeriod: time.Hour}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Append(message("session:a", "two")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	records, _, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d", len(records))
	}
}

func TestJournalReportsAPartialTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-a.jsonl")
	writeJournalRecords(t, path, []Record{
		{Stream: "session:a", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{"text":"one"}`)},
	})
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"stream":"session:a","seq":2`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	records, partial, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	if len(partial) == 0 {
		t.Fatal("the partial line was not reported")
	}
	if err := TruncatePartialJournal(path, partial); err != nil {
		t.Fatal(err)
	}
	records, partial, err = ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 0 || len(records) != 1 {
		t.Fatalf("after truncation records = %d partial = %q", len(records), partial)
	}
	// Appending after the truncation must land on a record boundary.
	sink, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(context.Background(), []Record{{
		Stream: "session:a", Seq: 2, Type: RecordMessage, Data: json.RawMessage(`{"text":"two"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	records, partial, err = ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || len(partial) != 0 {
		t.Fatalf("records = %d partial = %q", len(records), partial)
	}
}

func TestReadJournalHandlesAMissingFile(t *testing.T) {
	records, partial, err := ReadJournal(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 || len(partial) != 0 {
		t.Fatalf("records = %d partial = %q", len(records), partial)
	}
}

func TestTruncatePartialJournalIgnoresAnEmptyTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-a.jsonl")
	writeJournalRecords(t, path, []Record{{
		Stream: "session:a", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{}`),
	}})
	if err := TruncatePartialJournal(path, nil); err != nil {
		t.Fatal(err)
	}
	records, _, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
}

func TestTruncatePartialJournalRejectsAMissingFile(t *testing.T) {
	err := TruncatePartialJournal(filepath.Join(t.TempDir(), "absent.jsonl"), []byte("x"))
	if err == nil {
		t.Fatal("a missing journal was truncated")
	}
}

func TestOpenJournalRejectsAnEmptyPath(t *testing.T) {
	if _, err := OpenJournal("  "); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

func TestJournalRejectsUseAfterClose(t *testing.T) {
	sink, err := OpenJournal(filepath.Join(t.TempDir(), "session-a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("a second close failed: %v", err)
	}
	if err := sink.Write(context.Background(), []Record{{
		Stream: "s", Type: RecordMessage, Data: json.RawMessage(`{}`),
	}}); !errors.Is(err, ErrBufferClosed) {
		t.Fatalf("write after close = %v", err)
	}
	if err := sink.Sync(context.Background()); !errors.Is(err, ErrBufferClosed) {
		t.Fatalf("sync after close = %v", err)
	}
}

func TestJournalIgnoresAnEmptyBatch(t *testing.T) {
	sink, err := OpenJournal(filepath.Join(t.TempDir(), "session-a.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	if err := sink.Write(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestJournalPathsFollowTheScopeLayout(t *testing.T) {
	root := t.TempDir()
	session, err := JournalFileName(root, "session:abc")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "sessions", "abc", "session.jsonl")
	if session != want {
		t.Fatalf("session path = %q, want %q", session, want)
	}
	events, err := JournalFileName(root, "events:abc")
	if err != nil {
		t.Fatal(err)
	}
	if events != filepath.Join(root, "sessions", "abc", "events.jsonl") {
		t.Fatalf("events path = %q", events)
	}
	broker, err := JournalFileName(root, "broker:journal")
	if err != nil {
		t.Fatal(err)
	}
	if broker != filepath.Join(root, "broker", "journal.jsonl") {
		t.Fatalf("broker path = %q", broker)
	}
	for _, stream := range []string{
		"session", "session:", ":abc", "session:a/b", `session:a\b`,
		"session:a:b", "session:..", "session:.", "unknown:abc", "session:a\x00b",
	} {
		if _, err := JournalFileName(root, stream); err == nil {
			t.Fatalf("stream %q was accepted", stream)
		}
	}
}

func TestDiscoverJournalsIgnoresUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	writeScopeJournal(t, root, "session:abc", []Record{messageRecord(1, "m1", "user", "x")})
	writeScopeJournal(t, root, "events:abc", []Record{messageRecord(1, "m1", "user", "x")})
	for _, relative := range []string{
		"notes.txt",
		"index.json",
		"no-separator.jsonl",
		filepath.Join("sessions", "notes.txt"),
		filepath.Join("sessions", "abc", "extra.jsonl"),
	} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	journals, err := discoverJournals(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(journals) != 2 {
		t.Fatalf("journals = %#v", journals)
	}
	if journals[0].stream != "events:abc" || journals[1].stream != "session:abc" {
		t.Fatalf("streams = %#v", journals)
	}
}

// writeScopeJournal writes records to the journal of a stream in a scope root.
func writeScopeJournal(t *testing.T, root, stream string, records []Record) string {
	t.Helper()
	path, err := journalPath(root, stream)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	sink, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestJournalRoundTripsEveryRecordType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events-abc.jsonl")
	types := []string{
		RecordMessage, RecordEvent, RecordToolCallState,
		RecordPartialStream, RecordBrokerMessage,
	}
	records := make([]Record, 0, len(types))
	for index, recordType := range types {
		records = append(records, Record{
			Stream: "events:abc",
			Seq:    int64(index + 1),
			Type:   recordType,
			Data:   json.RawMessage(`{"index":` + string(rune('0'+index)) + `}`),
		})
	}
	writeJournalRecords(t, path, records)
	parsed, partial, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 0 {
		t.Fatalf("partial = %q", partial)
	}
	for index, record := range parsed {
		if record.Type != types[index] || record.Seq != int64(index+1) {
			t.Fatalf("record %d = %#v", index, record)
		}
		var payload map[string]int
		if err := json.Unmarshal(record.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["index"] != index {
			t.Fatalf("record %d payload = %#v", index, payload)
		}
	}
}
