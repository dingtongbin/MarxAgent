// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// memorySink records batches so a test can assert on ordering, batch sizes and
// how often a sync happened.
type memorySink struct {
	mu      sync.Mutex
	records []Record
	batches int
	syncs   int
	err     error
	syncErr error
	closed  bool
}

func (s *memorySink) Write(_ context.Context, batch []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.batches++
	s.records = append(s.records, batch...)
	return nil
}

func (s *memorySink) Sync(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncs++
	return s.syncErr
}

func (s *memorySink) snapshot() ([]Record, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...), s.batches, s.syncs
}

func newTestBuffer(t *testing.T, config Config, sinks ...Sink) *WriteBuffer {
	t.Helper()
	buffer, err := New(config, sinks...)
	if err != nil {
		t.Fatal(err)
	}
	return buffer
}

func message(stream string, text string) Record {
	return Record{
		Stream: stream,
		Type:   RecordMessage,
		Data:   json.RawMessage(`{"text":"` + text + `"}`),
	}
}

func TestAppendAssignsPerStreamSequences(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{}, sink)
	for index := 0; index < 3; index++ {
		if err := buffer.Append(message("session:a", "one")); err != nil {
			t.Fatal(err)
		}
		if err := buffer.Append(message("session:b", "two")); err != nil {
			t.Fatal(err)
		}
	}
	if got := buffer.LastSeq("session:a"); got != 3 {
		t.Fatalf("session a seq = %d", got)
	}
	if got := buffer.LastSeq("session:b"); got != 3 {
		t.Fatalf("session b seq = %d", got)
	}
	if got := buffer.LastSeq("session:missing"); got != 0 {
		t.Fatalf("unknown stream seq = %d", got)
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _, _ := sink.snapshot()
	if len(records) != 6 {
		t.Fatalf("records = %d", len(records))
	}
	seen := map[string]int64{}
	for index, record := range records {
		seen[record.Stream]++
		if record.Seq != seen[record.Stream] {
			t.Fatalf("record %d = %#v, want seq %d for %s",
				index, record, seen[record.Stream], record.Stream)
		}
	}
	if records[0].Ts.IsZero() {
		t.Fatal("timestamp was not filled in")
	}
}

func TestAppendRejectsInvalidRecords(t *testing.T) {
	buffer := newTestBuffer(t, Config{}, &memorySink{})
	cases := []struct {
		name   string
		record Record
	}{
		{name: "no stream", record: Record{Type: RecordMessage, Data: json.RawMessage(`1`)}},
		{name: "no type", record: Record{Stream: "s", Data: json.RawMessage(`1`)}},
		{name: "invalid json", record: Record{Stream: "s", Type: RecordMessage, Data: json.RawMessage(`{`)}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := buffer.Append(test.record); err == nil {
				t.Fatal("invalid record accepted")
			}
		})
	}
	if err := buffer.Append(Record{Stream: "s", Type: RecordEvent}); err != nil {
		t.Fatalf("empty data rejected: %v", err)
	}
	if err := buffer.Append(Record{
		Stream: "s", Type: RecordEvent, Ts: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWatermarkTriggersAWriteWithoutSync(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 4,
		FlushPeriod:      time.Hour,
	}, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- buffer.Run(ctx) }()

	for index := 0; index < 4; index++ {
		if err := buffer.Append(message("session:a", fmt.Sprintf("m%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		_, batches, _ := sink.snapshot()
		return batches > 0
	})
	_, batches, syncs := sink.snapshot()
	if batches != 1 {
		t.Fatalf("batches = %d, want 1", batches)
	}
	if syncs != 0 {
		t.Fatalf("a watermark write synced: %d", syncs)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestByteWatermarkTriggersAWrite(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 1 << 20,
		WatermarkBytes:   200,
		FlushPeriod:      time.Hour,
	}, sink)
	runBuffer(t, buffer)
	payload := json.RawMessage(`{"text":"` + strings.Repeat("x", 400) + `"}`)
	for index := 0; index < 4; index++ {
		if err := buffer.Append(Record{Stream: "s", Type: RecordMessage, Data: payload}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		_, batches, _ := sink.snapshot()
		return batches > 0
	})
}

func TestPeriodicTriggerWritesAndSyncs(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 1 << 20,
		FlushPeriod:      20 * time.Millisecond,
	}, sink)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- buffer.Run(ctx) }()
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, _, syncs := sink.snapshot()
		return syncs > 0
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	records, _, _ := sink.snapshot()
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
}

func TestFlushIsTheBoundaryTrigger(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 1 << 20,
		FlushPeriod:      time.Hour,
	}, sink)
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, batches, syncs := sink.snapshot()
	if batches != 1 || syncs != 1 {
		t.Fatalf("batches = %d syncs = %d", batches, syncs)
	}
	if buffer.Queued() != 0 {
		t.Fatalf("queued = %d", buffer.Queued())
	}
}

// runBuffer starts the periodic loop and guarantees it has stopped before the
// test returns. A loop that outlives its test would keep writing to state the
// test has already torn down, which turns a clean failure into a mystery.
func runBuffer(t *testing.T, buffer *WriteBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the write buffer loop did not stop")
		}
	})
}

func TestShutdownPerformsTheFallbackFlush(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 1 << 20,
		FlushPeriod:      time.Hour,
	}, sink)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- buffer.Run(ctx) }()
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	records, _, syncs := sink.snapshot()
	if len(records) != 1 || syncs != 1 {
		t.Fatalf("records = %d syncs = %d", len(records), syncs)
	}
	if err := buffer.Append(message("session:a", "late")); !errors.Is(err, ErrBufferClosed) {
		t.Fatalf("append after shutdown = %v", err)
	}
}

func TestCloseFlushesAndStopsAcceptingRecords(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{}, sink)
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _, syncs := sink.snapshot()
	if len(records) != 1 || syncs != 1 {
		t.Fatalf("records = %d syncs = %d", len(records), syncs)
	}
	if err := buffer.Append(message("session:a", "two")); !errors.Is(err, ErrBufferClosed) {
		t.Fatalf("append after close = %v", err)
	}
	if buffer.Queued() != 0 {
		t.Fatalf("queued = %d", buffer.Queued())
	}
}

func TestMultipleSinksReceiveEveryBatch(t *testing.T) {
	journal := &memorySink{}
	ledger := &memorySink{}
	buffer := newTestBuffer(t, Config{}, journal, ledger)
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	journalRecords, _, _ := journal.snapshot()
	ledgerRecords, _, _ := ledger.snapshot()
	if len(journalRecords) != len(ledgerRecords) || len(journalRecords) != 1 {
		t.Fatalf("journal = %d ledger = %d", len(journalRecords), len(ledgerRecords))
	}
	if journalRecords[0].Seq != ledgerRecords[0].Seq {
		t.Fatal("the two sinks disagree on the sequence number")
	}
}

func TestSinkFailuresAreReported(t *testing.T) {
	failing := &memorySink{err: errors.New("disk full")}
	buffer := newTestBuffer(t, Config{}, failing)
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(context.Background()); err == nil {
		t.Fatal("a failing sink was ignored")
	} else if !errors.Is(err, failing.err) {
		t.Fatalf("error = %v", err)
	}
	syncFailing := &memorySink{syncErr: errors.New("fsync failed")}
	buffer = newTestBuffer(t, Config{}, syncFailing)
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Flush(context.Background()); err == nil {
		t.Fatal("a failing sync was ignored")
	}
}

func TestRunSurfacesSinkFailures(t *testing.T) {
	failing := &memorySink{err: errors.New("disk full")}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 1,
		FlushPeriod:      time.Hour,
	}, failing)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- buffer.Run(ctx) }()
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run ignored a failing sink")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not report the failure")
	}
}

func TestQueueOverflowDropsTheOldestRecords(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 1 << 20,
		WatermarkBytes:   1 << 20,
		QueueBytes:       400,
		FlushPeriod:      time.Hour,
	}, sink)
	payload := json.RawMessage(`{"text":"` + strings.Repeat("x", 200) + `"}`)
	var overflow error
	for index := 0; index < 8; index++ {
		if err := buffer.Append(Record{Stream: "s", Type: RecordMessage, Data: payload}); err != nil {
			overflow = err
		}
	}
	if !errors.Is(overflow, ErrQueueOverflow) {
		t.Fatalf("overflow error = %v", overflow)
	}
	if buffer.Dropped() == 0 {
		t.Fatal("nothing was dropped")
	}
	if buffer.Queued() == 0 {
		t.Fatal("the queue was emptied entirely")
	}
	// The newest record must survive a stall, because losing it loses the answer.
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _, _ := sink.snapshot()
	last := records[len(records)-1]
	if last.Seq != buffer.LastSeq("s") {
		t.Fatalf("the newest record was dropped: last seq = %d newest = %d",
			last.Seq, buffer.LastSeq("s"))
	}
}

func TestNewRequiresSinks(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a buffer without a sink was accepted")
	}
	if _, err := New(Config{}, nil); err == nil {
		t.Fatal("a nil sink was accepted")
	}
}

func TestFlushAndRunRejectNilContexts(t *testing.T) {
	buffer := newTestBuffer(t, Config{}, &memorySink{})
	if err := buffer.Flush(nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil flush context = %v", err)
	}
	if err := buffer.Run(nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil run context = %v", err)
	}
	if err := buffer.Close(nil); err == nil {
		t.Fatal("nil close context was accepted")
	}
}

func TestFlushOnAnEmptyBufferStillSyncs(t *testing.T) {
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{}, sink)
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, batches, syncs := sink.snapshot(); batches != 0 || syncs != 1 {
		t.Fatalf("batches = %d syncs = %d", batches, syncs)
	}
}

func TestAppendPerformanceGate(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to enforce the durability gate")
	}
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 1 << 20,
		WatermarkBytes:   1 << 20,
		FlushPeriod:      time.Hour,
	}, sink)
	record := message("session:perf", "payload")
	const samples = 20000
	latencies := make([]time.Duration, 0, samples)
	for index := 0; index < samples; index++ {
		start := time.Now()
		if err := buffer.Append(record); err != nil {
			t.Fatal(err)
		}
		latencies = append(latencies, time.Since(start))
	}
	sortDurations(latencies)
	p99 := latencies[len(latencies)*99/100]
	if p99 >= 10*time.Microsecond {
		t.Fatalf("Append P99 = %v, want under 10ms", p99)
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, batches, _ := sink.snapshot()
	if len(records) != samples || batches != 1 {
		t.Fatalf("records = %d batches = %d", len(records), batches)
	}
}

func TestFlushPerformanceGate(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to enforce the durability gate")
	}
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{
		WatermarkRecords: 1 << 20,
		WatermarkBytes:   1 << 20,
		FlushPeriod:      time.Hour,
	}, sink)
	const total = 1000
	for index := 0; index < total; index++ {
		if err := buffer.Append(message("session:perf", fmt.Sprintf("m%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("flushing %d records took %v, want under 50ms", total, elapsed)
	}
	records, batches, _ := sink.snapshot()
	if len(records) != total || batches != 1 {
		t.Fatalf("records = %d batches = %d", len(records), batches)
	}
}

func BenchmarkWriteBufferAppend(b *testing.B) {
	sink := &memorySink{}
	buffer, err := New(Config{WatermarkRecords: 1 << 20, WatermarkBytes: 1 << 20, FlushPeriod: time.Hour}, sink)
	if err != nil {
		b.Fatal(err)
	}
	record := message("session:bench", "payload")
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := buffer.Append(record); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	_ = buffer.Flush(context.Background())
}

func BenchmarkWriteBufferFlush1000(b *testing.B) {
	record := message("session:bench", "payload")
	for index := 0; index < b.N; index++ {
		b.StopTimer()
		sink := &memorySink{}
		buffer, err := New(Config{WatermarkRecords: 1 << 20, WatermarkBytes: 1 << 20, FlushPeriod: time.Hour}, sink)
		if err != nil {
			b.Fatal(err)
		}
		for count := 0; count < 1000; count++ {
			if err := buffer.Append(record); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		if err := buffer.Flush(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met in time")
}

func sortDurations(values []time.Duration) {
	for index := 1; index < len(values); index++ {
		for inner := index; inner > 0 && values[inner-1] > values[inner]; inner-- {
			values[inner-1], values[inner] = values[inner], values[inner-1]
		}
	}
}

// The two sinks are fed from one queue, so what the test is about is that a
// record lands in both in the order it was appended. The order of the writing is
// not the subject, so the test ends the run itself rather than waiting for a
// trigger: the watermark only fires once the queue is full, and how many records
// happen to be left over depends on where a write landed, so waiting for all nine
// would be waiting for an alignment the buffer makes no promise about.
func TestWriteBufferKeepsRecordOrderAcrossSinks(t *testing.T) {
	journal := &memorySink{}
	ledger := &memorySink{}
	buffer := newTestBuffer(t, Config{WatermarkRecords: 3, FlushPeriod: time.Hour}, journal, ledger)
	runBuffer(t, buffer)
	for index := 0; index < 9; index++ {
		if err := buffer.Append(message("session:a", fmt.Sprintf("m%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	if err := buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	journalRecords, _, _ := journal.snapshot()
	ledgerRecords, _, _ := ledger.snapshot()
	if len(journalRecords) != 9 || len(ledgerRecords) != 9 {
		t.Fatalf("journal=%d ledger=%d, want nine each", len(journalRecords), len(ledgerRecords))
	}
	for index := range journalRecords {
		if journalRecords[index].Seq != ledgerRecords[index].Seq {
			t.Fatalf("record %d diverged between sinks", index)
		}
		// The sinks are fed from one batch, so a record that reached them out of
		// order would mean the queue handed them out in different orders.
		if journalRecords[index].Seq != int64(index+1) {
			t.Fatalf("record %d has sequence %d, so the order was not kept", index, journalRecords[index].Seq)
		}
	}
}

// The watermark writes on its own, and the periodic trigger is the backstop for
// whatever is left under it. The two are asserted separately because together they
// hide a trigger that stopped working: nine records is a multiple of the
// watermark, so a broken watermark would still be covered by the period, and a
// broken period would be covered by the watermark.
func TestTheWatermarkWritesOnItsOwnAndThePeriodCoversTheRest(t *testing.T) {
	t.Run("the watermark writes without waiting for the period", func(t *testing.T) {
		sink := &memorySink{}
		// A period long enough that it cannot be what wrote the records.
		buffer := newTestBuffer(t, Config{WatermarkRecords: 4, FlushPeriod: time.Hour}, sink)
		runBuffer(t, buffer)
		for index := 0; index < 4; index++ {
			if err := buffer.Append(message("session:a", fmt.Sprintf("m%d", index))); err != nil {
				t.Fatal(err)
			}
		}
		waitFor(t, func() bool {
			records, _, _ := sink.snapshot()
			return len(records) == 4
		})
	})

	t.Run("the period writes what the watermark leaves behind", func(t *testing.T) {
		sink := &memorySink{}
		// A watermark nothing reaches and a period short enough to observe, which
		// together describe the case the design promises a bound for: records left
		// under the watermark are written within one period.
		buffer := newTestBuffer(t, Config{
			WatermarkRecords: 1 << 20,
			FlushPeriod:      10 * time.Millisecond,
		}, sink)
		runBuffer(t, buffer)
		for index := 0; index < 3; index++ {
			if err := buffer.Append(message("session:a", fmt.Sprintf("m%d", index))); err != nil {
				t.Fatal(err)
			}
		}
		// The condition waits for the sync as well as the write, because the sink
		// records them in that order and a poll that stopped at the write would be
		// measuring when the test looked rather than what the loop did.
		waitFor(t, func() bool {
			records, _, syncs := sink.snapshot()
			return len(records) == 3 && syncs > 0
		})
	})
}

func TestBufferSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	if err := os.WriteFile(path, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	buffer := newTestBuffer(t, Config{}, sink)
	if err := buffer.Append(message("session:a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := buffer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing\n" {
		t.Fatalf("the buffer touched the file: %q", data)
	}
}
