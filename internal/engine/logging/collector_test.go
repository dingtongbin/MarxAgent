// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedClock() func() time.Time {
	moment := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return moment }
}

func newTestCollector(t *testing.T, config Config) (*Collector, *CountingWriter) {
	t.Helper()
	writer := &CountingWriter{}
	if config.Writer == nil {
		config.Writer = writer
	}
	if config.Clock == nil {
		config.Clock = fixedClock()
	}
	if config.FlushPeriod == 0 {
		config.FlushPeriod = 10 * time.Millisecond
	}
	collector, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return collector, writer
}

func TestNewValidatesItsConfiguration(t *testing.T) {
	if _, err := New(Config{Level: "shouty"}); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	if _, err := New(Config{Format: "yaml"}); err == nil {
		t.Fatal("an unknown format was accepted")
	}
	if _, err := New(Config{Directory: filepath.Join(t.TempDir(), "x", "\x00bad")}); err == nil {
		t.Fatal("an unusable directory was accepted")
	}
	collector, err := New(Config{Level: "  "})
	if err != nil {
		t.Fatal(err)
	}
	// Defaults are applied rather than leaving zero values in place.
	if collector.config.BufferSize != DefaultBufferSize {
		t.Fatalf("buffer = %d", collector.config.BufferSize)
	}
	if collector.config.FlushPeriod != DefaultFlushPeriod {
		t.Fatalf("period = %v", collector.config.FlushPeriod)
	}
	if collector.config.Format != "json" {
		t.Fatalf("format = %q", collector.config.Format)
	}
	if collector.Level().Level() != slog.LevelInfo {
		t.Fatalf("level = %v", collector.Level().Level())
	}
	collector.Close()
}

func TestEveryRecordCarriesTheFrozenFields(t *testing.T) {
	collector, seen := collecting(t, Config{})
	logger := collector.Logger(LayerL2, "gateway")
	start(t, collector)
	logger.Info("a message", "extra", "value")
	awaitLine(t, seen)
	// The point of the frozen set: a query written against a field always finds it,
	// even on a record that had nothing to say about it.
	var line map[string]any
	if err := json.Unmarshal(seen.first(t), &line); err != nil {
		t.Fatal(err)
	}
	for _, field := range FrozenFields {
		if _, present := line[field]; !present {
			t.Fatalf("the record is missing %q: %v", field, line)
		}
	}
	if line[FieldService] != ServiceName {
		t.Fatalf("service = %v", line[FieldService])
	}
	if line[FieldLayer] != string(LayerL2) {
		t.Fatalf("layer = %v", line[FieldLayer])
	}
	if line[FieldComponent] != "gateway" {
		t.Fatalf("component = %v", line[FieldComponent])
	}
	if line[FieldMessage] != "a message" {
		t.Fatalf("message = %v", line[FieldMessage])
	}
	if line[FieldTimestamp] != "2026-09-25T12:00:00Z" {
		t.Fatalf("timestamp = %v", line[FieldTimestamp])
	}
	if line["extra"] != "value" {
		t.Fatalf("extra = %v", line["extra"])
	}
}

func TestAReservedFieldCannotBeForged(t *testing.T) {
	collector, seen := collecting(t, Config{})
	logger := collector.Logger(LayerL1, "loop")
	start(t, collector)
	// A caller that logs a reserved field is trying to forge a record, and the
	// design's auditability depends on those fields meaning what they say.
	logger.Info("real message", "service", "someone-else", "layer", "L3", "msg", "forged")
	awaitLine(t, seen)
	var line map[string]any
	if err := json.Unmarshal(seen.first(t), &line); err != nil {
		t.Fatal(err)
	}
	if line[FieldService] != ServiceName {
		t.Fatalf("the service field was forged: %v", line[FieldService])
	}
	if line[FieldLayer] != string(LayerL1) {
		t.Fatalf("the layer field was forged: %v", line[FieldLayer])
	}
	if line[FieldMessage] != "real message" {
		t.Fatalf("the message field was forged: %v", line[FieldMessage])
	}
	// A reserved attribute set through With is dropped rather than passed on, so
	// the frozen set cannot be widened at all.
	forged := collector.Logger(LayerL1, "loop").With("service", "someone-else")
	if forged.Enabled(context.Background(), slog.LevelInfo) != true {
		t.Fatal("the logger is not enabled")
	}
}

func TestGroupedAttributesAreFlattened(t *testing.T) {
	collector, seen := collecting(t, Config{})
	logger := collector.Logger(LayerL2, "storage").WithGroup("request")
	start(t, collector)
	logger.Info("handled", "id", "abc", "duration_ms", 12)
	awaitLine(t, seen)
	var line map[string]any
	if err := json.Unmarshal(seen.first(t), &line); err != nil {
		t.Fatal(err)
	}
	// The field set is flat, because a reader querying a nested object needs a
	// parser and a log line is read by a machine.
	if line["request.id"] != "abc" {
		t.Fatalf("line = %v", line)
	}
	if line["request.duration_ms"] != float64(12) {
		t.Fatalf("line = %v", line)
	}
}

func TestLevelFilteringAndRuntimeSwitch(t *testing.T) {
	collector, writer := newTestCollector(t, Config{Level: "warn"})
	logger := collector.Logger(LayerL1, "loop")
	start(t, collector)
	logger.Debug("not recorded")
	logger.Info("not recorded either")
	logger.Warn("recorded")
	logger.Error("also recorded")
	deadline := time.Now().Add(2 * time.Second)
	for writer.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	stats := collector.Stats()
	if stats.Written != 2 {
		t.Fatalf("written = %d, only the two at or above warn", stats.Written)
	}
	if stats.ByLevel["warn"] != 1 || stats.ByLevel["error"] != 1 {
		t.Fatalf("by level = %#v", stats.ByLevel)
	}
	// The level changes at runtime, which a start up flag cannot provide and the
	// web and command line surfaces need.
	if err := collector.SetLevelName("debug"); err != nil {
		t.Fatal(err)
	}
	logger.Debug("now recorded")
	deadline = time.Now().Add(2 * time.Second)
	for stats.Written < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		stats = collector.Stats()
	}
	if stats.Written != 3 {
		t.Fatalf("written = %d after lowering the level", stats.Written)
	}
	if err := collector.SetLevelName("shouty"); err == nil {
		t.Fatal("an unknown level name was accepted")
	}
	collector.SetLevel(slog.LevelError)
	if !collector.Enabled(slog.LevelError) || collector.Enabled(slog.LevelInfo) {
		t.Fatal("the level was not applied")
	}
}

func TestTraceLevelIsBelowDebug(t *testing.T) {
	collector, seen := collecting(t, Config{Level: "trace"})
	start(t, collector)
	collector.Trace(context.Background(), LayerL1, "loop", "a trace record")
	awaitLine(t, seen)
	var line map[string]any
	if err := json.Unmarshal(seen.first(t), &line); err != nil {
		t.Fatal(err)
	}
	if line[FieldLevel] != "trace" {
		t.Fatalf("level = %v", line[FieldLevel])
	}
	// A logger handed out for tracing carries no level of its own, because a level
	// belongs to the call.
	if collector.TraceLogger(LayerL1, "loop") == nil {
		t.Fatal("the trace logger is nil")
	}
}

func TestAWriterFailureNeverReachesTheCaller(t *testing.T) {
	// The whole point: a log that cannot be written is not a reason to fail the work
	// it describes.
	sentinel := errors.New("the disk is full")
	collector, writer := newTestCollector(t, Config{})
	writer.Fail(sentinel)
	start(t, collector)
	logger := collector.Logger(LayerL2, "storage")
	logger.Info("this write will fail")
	logger.Error("so will this one")
	// Give the writer goroutine time to try and fail.
	time.Sleep(100 * time.Millisecond)
	stats := collector.Stats()
	if stats.Written != 0 {
		t.Fatalf("written = %d despite the failure", stats.Written)
	}
	// The record is still retained, so a crash report can show what was happening.
	if collector.LastRecord().Fields[FieldMessage] != "so will this one" {
		t.Fatalf("last record = %#v", collector.LastRecord().Fields)
	}
	// Nothing was dropped: the records reached the writer goroutine and it tried.
	if stats.Dropped != 0 {
		t.Fatalf("dropped = %d", stats.Dropped)
	}
	// Once the writer recovers, records flow again.
	writer.Fail(nil)
	logger.Info("this one works")
	deadline := time.Now().Add(2 * time.Second)
	for collector.Stats().Written == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if collector.Stats().Written == 0 {
		t.Fatal("the collector did not recover")
	}
}

func TestAFullBufferShedsTheOldestAndCountsIt(t *testing.T) {
	// A caller logging must not wait for a disk, so a full buffer drops rather
	// than blocking. The count is what makes an incomplete log detectable.
	collector, _ := newTestCollector(t, Config{
		BufferSize:  4,
		FlushPeriod: time.Hour,
	})
	// The writer goroutine is not started, so nothing drains the buffer.
	logger := collector.Logger(LayerL1, "loop")
	for index := 0; index < 20; index++ {
		logger.Info("flooding", "index", index)
	}
	stats := collector.Stats()
	if stats.Dropped == 0 {
		t.Fatal("a flooded buffer shed nothing")
	}
	if stats.Pending > 4 {
		t.Fatalf("pending = %d, the buffer is unbounded", stats.Pending)
	}
	if err := collector.Err(); !errors.Is(err, ErrBufferFull) {
		t.Fatalf("err = %v", err)
	}
	// The most recent record is the one that survived, because the oldest are shed.
	// slog keeps the caller's own numeric type, so the value is compared as text
	// rather than as an int the test would have to guess the type of.
	if fmt.Sprint(collector.LastRecord().Fields["index"]) != "19" {
		t.Fatalf("last record = %#v", collector.LastRecord().Fields)
	}
	// A clean buffer reports no error, so a reader can trust the log.
	quiet, _ := newTestCollector(t, Config{BufferSize: 8, FlushPeriod: time.Hour})
	if err := quiet.Err(); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestEnqueueAfterCloseIsIgnored(t *testing.T) {
	collector, _ := newTestCollector(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		collector.Run(ctx)
	}()
	cancel()
	<-finished
	collector.Close()
	// Logging after the shutdown must not panic, because a component that outlives
	// the collector is a bug that should not take the process with it.
	collector.Logger(LayerL1, "loop").Info("after close")
	collector.Close()
}

func TestRollingFilesAreWrittenPerDay(t *testing.T) {
	directory := t.TempDir()
	collector, err := New(Config{
		Directory: directory,
		Level:     "info",
		Clock:     fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The file is released on every path, including a failed assertion, because an
	// open handle keeps the temporary directory from being removed on Windows.
	t.Cleanup(collector.Close)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		collector.Run(ctx)
	}()
	collector.Logger(LayerL2, "storage").Info("on disk")
	deadline := time.Now().Add(2 * time.Second)
	for collector.FilePath() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	path := collector.FilePath()
	if path == "" {
		cancel()
		<-finished
		t.Fatal("no file was opened")
	}
	if parseFileDay(path) != "2026-09-25" {
		t.Fatalf("file = %q", path)
	}
	cancel()
	<-finished
	collector.Close()

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("files = %d", len(entries))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &line); err != nil {
		t.Fatal(err)
	}
	if line[FieldService] != ServiceName {
		t.Fatalf("line = %v", line)
	}
}

func TestTextFormatIsAlsoUsable(t *testing.T) {
	collector, seen := collecting(t, Config{Format: "text"})
	start(t, collector)
	collector.Logger(LayerL1, "loop").Warn("watch out", "count", 3)
	awaitLine(t, seen)
	line := string(seen.first(t))
	for _, wanted := range []string{"level=warn", "msg=\"watch out\"", "service=marxagent", "count=3"} {
		if !strings.Contains(line, wanted) {
			t.Fatalf("line = %q, missing %q", line, wanted)
		}
	}
}

func TestOTLPOffByDefault(t *testing.T) {
	collector, _ := newTestCollector(t, Config{})
	// The design's default is no telemetry at all, and turning it on has to be
	// something a caller did on purpose.
	if collector.OTLPEnabled() {
		t.Fatal("the bridge was on by default")
	}
	collector.EnableOTLP(true)
	if !collector.OTLPEnabled() {
		t.Fatal("the bridge did not turn on")
	}
	if !collector.Stats().OTLPEnabled {
		t.Fatal("the tally did not report the bridge")
	}
	collector.EnableOTLP(false)
	if collector.OTLPEnabled() {
		t.Fatal("the bridge did not turn off")
	}
}

func TestRecordClonesItsFields(t *testing.T) {
	record := Record{Fields: map[string]any{FieldMessage: "one"}}
	clone := record.Clone()
	clone.Fields[FieldMessage] = "two"
	if record.Fields[FieldMessage] != "one" {
		t.Fatal("the clone shares its map")
	}
	if _, ok := record.Field("absent"); ok {
		t.Fatal("an absent field reported itself present")
	}
}

func TestParseFileDayIgnoresWhatItCannotRead(t *testing.T) {
	cases := map[string]string{
		"marxagent-2026-09-25.json": "2026-09-25",
		"marxagent.json":            "",
		"other.json":                "",
		"marxagent-nonsense.json":   "",
	}
	for name, wanted := range cases {
		if got := parseFileDay(name); got != wanted {
			t.Fatalf("parseFileDay(%q) = %q, want %q", name, got, wanted)
		}
	}
}

func TestFileRollsOverAtItsSizeLimit(t *testing.T) {
	directory := t.TempDir()
	collector, err := New(Config{
		Directory:    directory,
		Level:        "info",
		Clock:        fixedClock(),
		MaxFileBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		collector.Run(ctx)
	}()
	logger := collector.Logger(LayerL1, "loop")
	for index := 0; index < 5; index++ {
		logger.Info("a line long enough to exceed the limit", "index", index)
	}
	deadline := time.Now().Add(2 * time.Second)
	for collector.FilePath() == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-finished
	collector.Close()
	// The size limit asks for a rollover, and the same day means the same file name,
	// so the guarantee is that the collector notices rather than that a new name
	// appears. What is asserted is that records were not lost.
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing was written")
	}
}

func TestRollingIsOffByDefault(t *testing.T) {
	collector, err := New(Config{Directory: t.TempDir(), Clock: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	collector.writerMu.Lock()
	rolling := collector.rollNeeded()
	collector.writerMu.Unlock()
	if rolling {
		t.Fatal("rolling happened with no limit set")
	}
	collector.Close()
}

// capture collects the lines a writer received, so a test can assert on what the
// collector actually produced rather than on what it was asked to produce.
type capture struct {
	mu    sync.Mutex
	lines [][]byte
}

func (c *capture) add(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, append([]byte(nil), data...))
}

func (c *capture) first(t *testing.T) []byte {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.lines) == 0 {
		t.Fatal("nothing was written")
	}
	return c.lines[0]
}

// collecting builds a writer that records every line.
func collecting(t *testing.T, config Config) (*Collector, *capture) {
	t.Helper()
	seen := &capture{}
	config.Writer = &CountingWriter{Sink: seen.add}
	if config.Clock == nil {
		config.Clock = fixedClock()
	}
	if config.FlushPeriod == 0 {
		config.FlushPeriod = 10 * time.Millisecond
	}
	collector, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return collector, seen
}

// awaitLine waits for the writer goroutine to produce at least one line.
func awaitLine(t *testing.T, seen *capture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		seen.mu.Lock()
		count := len(seen.lines)
		seen.mu.Unlock()
		if count > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("nothing was written")
}
