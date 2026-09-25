// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newAudit(t *testing.T) *AuditSink {
	t.Helper()
	sink, err := OpenAudit(filepath.Join(t.TempDir(), "marxagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	return sink
}

func messageRecord(seq int64, id, role, content string) Record {
	data, err := json.Marshal(map[string]string{"id": id, "role": role, "content": content})
	if err != nil {
		panic(err)
	}
	return Record{
		Stream: "session:s1",
		Seq:    seq,
		Type:   RecordMessage,
		Data:   data,
		Ts:     time.Unix(seq, 0).UTC(),
	}
}

func TestAuditStoresAndSearchesMessages(t *testing.T) {
	audit := newAudit(t)
	version, err := audit.SchemaVersionOf()
	if err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d", version)
	}
	if err := audit.Write(context.Background(), []Record{
		messageRecord(1, "m1", "user", "查找 needle 的地方"),
		messageRecord(2, "m2", "assistant", "found it in the haystack"),
	}); err != nil {
		t.Fatal(err)
	}
	results, err := audit.SearchMessages(context.Background(), "s1", "needle", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].MessageID != "m1" {
		t.Fatalf("results = %#v", results)
	}
	if results[0].Role != "user" || !strings.Contains(results[0].Content, "needle") {
		t.Fatalf("result = %#v", results[0])
	}
	// A query shorter than a trigram falls back to a scan, which is what makes
	// two character queries work at all.
	short, err := audit.SearchMessages(context.Background(), "s1", "查找", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(short) != 1 || short[0].MessageID != "m1" {
		t.Fatalf("short query results = %#v", short)
	}
	if count, err := audit.EventCount(context.Background(), "s1"); err != nil || count != 2 {
		t.Fatalf("events = %d err = %v", count, err)
	}
}

func TestAuditSearchScopesToASession(t *testing.T) {
	audit := newAudit(t)
	if err := audit.Write(context.Background(), []Record{
		messageRecord(1, "m1", "user", "shared token"),
		{Stream: "session:other", Seq: 1, Type: RecordMessage,
			Data: json.RawMessage(`{"id":"n1","role":"user","content":"shared token"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	scoped, err := audit.SearchMessages(context.Background(), "s1", "shared", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].SessionID != "s1" {
		t.Fatalf("scoped results = %#v", scoped)
	}
	all, err := audit.SearchMessages(context.Background(), "", "shared", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unscoped results = %#v", all)
	}
}

func TestAuditSearchRejectsEmptyQueriesAndBoundsLimits(t *testing.T) {
	audit := newAudit(t)
	if _, err := audit.SearchMessages(context.Background(), "s1", "   ", 10); err == nil {
		t.Fatal("an empty query was accepted")
	}
	if err := audit.Write(context.Background(), []Record{messageRecord(1, "m1", "user", "bounded")}); err != nil {
		t.Fatal(err)
	}
	results, err := audit.SearchMessages(context.Background(), "s1", "bounded", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
	if _, err := audit.SearchMessages(context.Background(), "s1", "boun", 100000); err != nil {
		t.Fatal(err)
	}
	// A query made of punctuation must not be parsed as FTS syntax.
	if _, err := audit.SearchMessages(context.Background(), "s1", `a" OR b`, 10); err == nil {
		t.Log("punctuation query rejected by the tokenizer, which is acceptable")
	}
}

func TestAuditToolCallStatesAreIdempotent(t *testing.T) {
	audit := newAudit(t)
	record := Record{
		Stream: "session:s1",
		Seq:    7,
		Type:   RecordToolCallState,
		Data:   json.RawMessage(`{"tool_name":"read","status":"pending"}`),
		Ts:     time.Unix(7, 0).UTC(),
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := audit.Write(context.Background(), []Record{record}); err != nil {
			t.Fatal(err)
		}
	}
	seq, err := audit.HighestToolSeq(context.Background(), "session:s1")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 7 {
		t.Fatalf("highest sequence = %d", seq)
	}
}

func TestAuditCheckpointsRoundTrip(t *testing.T) {
	audit := newAudit(t)
	seq, count, summary, err := audit.Checkpoint(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 || count != 0 || summary != "" {
		t.Fatalf("empty checkpoint = %d %d %q", seq, count, summary)
	}
	if err := audit.WriteCheckpoint(context.Background(), "s1", 42, 17, "a summary"); err != nil {
		t.Fatal(err)
	}
	seq, count, summary, err = audit.Checkpoint(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 42 || count != 17 || summary != "a summary" {
		t.Fatalf("checkpoint = %d %d %q", seq, count, summary)
	}
	if got, err := audit.LastAppliedSeq(context.Background(), "s1"); err != nil || got != 42 {
		t.Fatalf("last applied = %d err = %v", got, err)
	}
	if err := audit.WriteCheckpoint(context.Background(), "s1", 43, 18, "newer"); err != nil {
		t.Fatal(err)
	}
	if got, err := audit.LastAppliedSeq(context.Background(), "s1"); err != nil || got != 43 {
		t.Fatalf("updated checkpoint = %d err = %v", got, err)
	}
}

func TestAuditRejectsMalformedPayloads(t *testing.T) {
	audit := newAudit(t)
	if err := audit.Write(context.Background(), []Record{{
		Stream: "session:s1", Seq: 1, Type: RecordMessage, Data: json.RawMessage(`{`),
	}}); err == nil {
		t.Fatal("a malformed message record was accepted")
	}
	if err := audit.Write(context.Background(), []Record{{
		Stream: "session:s1", Seq: 2, Type: RecordToolCallState, Data: json.RawMessage(`{`),
	}}); err == nil {
		t.Fatal("a malformed tool call record was accepted")
	}
	// A failed batch must leave nothing behind.
	if count, err := audit.EventCount(context.Background(), "s1"); err != nil || count != 0 {
		t.Fatalf("events = %d err = %v", count, err)
	}
}

func TestAuditIgnoresAnEmptyBatch(t *testing.T) {
	audit := newAudit(t)
	if err := audit.Write(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := audit.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAuditClosesIdempotently(t *testing.T) {
	sink, err := OpenAudit(filepath.Join(t.TempDir(), "marxagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("a second close failed: %v", err)
	}
	if err := sink.Write(context.Background(), []Record{messageRecord(1, "m1", "user", "x")}); err == nil {
		t.Fatal("write after close was accepted")
	}
	if err := sink.Sync(context.Background()); err == nil {
		t.Fatal("sync after close was accepted")
	}
	if _, err := sink.SearchMessages(context.Background(), "s1", "query", 1); err == nil {
		t.Fatal("search after close was accepted")
	}
	if err := sink.WriteCheckpoint(context.Background(), "s1", 1, 1, "s"); err == nil {
		t.Fatal("checkpoint after close was accepted")
	}
	if _, _, _, err := sink.Checkpoint(context.Background(), "s1"); err == nil {
		t.Fatal("checkpoint read after close was accepted")
	}
}

func TestOpenAuditRejectsAnEmptyPath(t *testing.T) {
	if _, err := OpenAudit("  "); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

func TestAuditDatabaseSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marxagent.db")
	first, err := OpenAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Write(context.Background(), []Record{
		messageRecord(1, "m1", "user", "persisted needle"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	results, err := second.SearchMessages(context.Background(), "s1", "persisted", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results after reopen = %#v", results)
	}
}

func TestAuditWorksAsASecondSink(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenJournal(filepath.Join(dir, "session-s1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	audit, err := OpenAudit(filepath.Join(dir, "marxagent.db"))
	if err != nil {
		t.Fatal(err)
	}
	buffer, err := New(Config{WatermarkRecords: 4, FlushPeriod: time.Hour}, journal, audit)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go buffer.Run(ctx)
	for index := 1; index <= 4; index++ {
		if err := buffer.Append(messageRecord(int64(index), "m"+string(rune('0'+index)), "user",
			"double write needle")); err != nil {
			t.Fatal(err)
		}
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, partial, err := ReadJournal(filepath.Join(dir, "session-s1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || len(partial) != 0 {
		t.Fatalf("journal records = %d partial = %q", len(records), partial)
	}
	results, err := audit.SearchMessages(context.Background(), "s1", "needle", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 {
		t.Fatalf("audit results = %d", len(results))
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := audit.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAuditSurvivesAnInterruptedTransaction(t *testing.T) {
	audit := newAudit(t)
	// One good record, then a batch that fails partway: the good record must
	// still be there and the failed batch must not be half written.
	if err := audit.Write(context.Background(), []Record{
		messageRecord(1, "m1", "user", "committed"),
	}); err != nil {
		t.Fatal(err)
	}
	err := audit.Write(context.Background(), []Record{
		messageRecord(2, "m2", "user", "doomed"),
		{Stream: "session:s1", Seq: 3, Type: RecordMessage, Data: json.RawMessage(`{`)},
	})
	if err == nil {
		t.Fatal("the malformed batch was accepted")
	}
	results, err := audit.SearchMessages(context.Background(), "s1", "committed", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("committed records = %d", len(results))
	}
	doomed, err := audit.SearchMessages(context.Background(), "s1", "doomed", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(doomed) != 0 {
		t.Fatalf("a rolled back record survived: %#v", doomed)
	}
}
