// SPDX-License-Identifier: Apache-2.0

// Package logging holds the log collector. Everything goes through log/slog, the
// collector aggregates and writes, and the OpenTelemetry bridge is off by default.
//
// The design makes two promises this package exists to keep: the field set is
// frozen, so a log line means the same thing in every version, and a logging
// failure never reaches the caller, because a full buffer or a broken file must
// not take down the turn that was merely being recorded.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ServiceName is fixed. A log line that did not say which program wrote it is
// useless once two are running.
const ServiceName = "marxagent"

// Layer names the tier a record came from.
type Layer string

const (
	// LayerL1 is the core loop.
	LayerL1 Layer = "L1"
	// LayerL2 is the engineering parts.
	LayerL2 Layer = "L2"
	// LayerL3 is the assembly and interface layer.
	LayerL3 Layer = "L3"
)

// The field names are frozen: only added to, never renamed or removed, because a
// query written against a field that changed meaning silently stops finding
// things.
const (
	FieldTimestamp = "ts"
	FieldLevel     = "level"
	FieldMessage   = "msg"
	FieldService   = "service"
	FieldLayer     = "layer"
	FieldComponent = "component"
	FieldSessionID = "session_id"
	FieldAgentID   = "agent_id"
	FieldTraceID   = "trace_id"
	FieldSpanID    = "span_id"
	FieldEventType = "event_type"
	FieldError     = "err"
)

// FrozenFields is the full set every record carries, in the order it is written.
var FrozenFields = []string{
	FieldTimestamp, FieldLevel, FieldMessage, FieldService, FieldLayer, FieldComponent,
	FieldSessionID, FieldAgentID, FieldTraceID, FieldSpanID, FieldEventType, FieldError,
}

// Record is one log line, as a flat map so the collector stays independent of any
// transport and the frozen fields can be enforced in one place.
type Record struct {
	Fields map[string]any
	// At is when the record was created, kept separately because the timestamp is
	// part of the frozen set and a caller must not be able to move it.
	At time.Time
}

// Clone returns a copy, so a record handed to a sink cannot be changed by whoever
// produced it.
func (r Record) Clone() Record {
	fields := make(map[string]any, len(r.Fields))
	for key, value := range r.Fields {
		fields[key] = value
	}
	return Record{Fields: fields, At: r.At}
}

// Field reads one field.
func (r Record) Field(name string) (any, bool) {
	value, ok := r.Fields[name]
	return value, ok
}

// Config configures a collector.
type Config struct {
	// Directory receives the rolling files. An empty directory sends records to
	// the writer instead, which is what a test or a piped run wants.
	Directory string
	// Level is the minimum level recorded. Empty selects info.
	Level string
	// Format is json or text. Empty selects json, because the design's output is
	// structured and a reader is a machine.
	Format string
	// BufferSize is how many records may wait to be written before the oldest are
	// dropped. Zero selects the default.
	BufferSize int
	// FlushPeriod is how often the buffer is written whether or not it is full.
	// Zero selects the default.
	FlushPeriod time.Duration
	// Writer receives records when no directory is set. It is a seam for a test and
	// for a piped run.
	Writer io.Writer
	// Clock is the time source, so a test can make records deterministic.
	Clock func() time.Time
	// MaxFileBytes rolls a file over at this size. Zero disables rolling.
	MaxFileBytes int64
}

const (
	// DefaultBufferSize is how many records may wait before the oldest are dropped.
	DefaultBufferSize = 4096
	// DefaultFlushPeriod is how often the buffer is written.
	DefaultFlushPeriod = time.Second
	// LevelTrace is the most verbose level the collector records.
	LevelTrace = slog.LevelDebug - 4
)

// ErrBufferFull is reported when the buffer shed records, so a caller can notice
// that the log is incomplete.
var ErrBufferFull = fmt.Errorf("logging: the buffer shed records")

// Collector aggregates records and writes them.
//
// Writes happen on its own goroutine. A caller logging must not wait for a disk,
// because the turn being recorded is worth more than the record of it.
type Collector struct {
	level  *slog.LevelVar
	config Config

	records chan Record
	// stop tells the writer goroutine to finish. It is separate from records
	// because closing the queue channel would make the goroutine's receive return
	// zero values in a hot loop, and because a collector whose goroutine was never
	// started still has to be closable.
	stop chan struct{}
	done chan struct{}
	// started records whether Run was called, so Close knows whether there is
	// anything to wait for.
	started atomic.Bool
	// writerMu guards the file handle, because a rollover swaps it underneath the
	// writer goroutine.
	writerMu sync.Mutex
	file     *os.File
	day      string
	external io.Writer

	closeOnce sync.Once
	stopped   atomic.Bool

	mu sync.Mutex
	// written and dropped are the counters a reader needs to know whether the log
	// can be trusted.
	written uint64
	dropped uint64
	// levelCounts tracks how many records each level produced, which is how a hot
	// loop announces itself.
	levelCounts map[string]uint64
	// lastRecord is retained for a test and for a crash report.
	lastRecord Record
	// otlpEnabled is the bridge flag, which the design keeps off by default.
	otlpEnabled bool
}

// New builds a collector.
func New(config Config) (*Collector, error) {
	if config.BufferSize <= 0 {
		config.BufferSize = DefaultBufferSize
	}
	if config.FlushPeriod <= 0 {
		config.FlushPeriod = DefaultFlushPeriod
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	level, err := parseLevel(config.Level)
	if err != nil {
		return nil, err
	}
	if config.Format == "" {
		config.Format = "json"
	}
	if config.Format != "json" && config.Format != "text" {
		return nil, fmt.Errorf("logging: unknown format %q", config.Format)
	}
	collector := &Collector{
		level:       &slog.LevelVar{},
		config:      config,
		records:     make(chan Record, config.BufferSize),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		levelCounts: map[string]uint64{},
	}
	collector.level.Set(level)
	if config.Directory != "" {
		if err := os.MkdirAll(config.Directory, 0o755); err != nil {
			return nil, fmt.Errorf("logging: create the log directory: %w", err)
		}
	}
	collector.external = config.Writer
	return collector, nil
}

// Level reports the current level, and the variable that changes it.
func (c *Collector) Level() *slog.LevelVar { return c.level }

// SetLevel changes the level at runtime, which the web and command line surfaces
// need and which a start up flag cannot provide.
func (c *Collector) SetLevel(level slog.Level) { c.level.Set(level) }

// SetLevelName changes the level by name.
func (c *Collector) SetLevelName(name string) error {
	level, err := parseLevel(name)
	if err != nil {
		return err
	}
	c.level.Set(level)
	return nil
}

// Logger returns a slog logger that feeds this collector, tagged with the fields
// every record from that component must carry.
func (c *Collector) Logger(layer Layer, component string) *slog.Logger {
	handler := &collectorHandler{collector: c, layer: layer, component: component}
	return slog.New(handler)
}

// TraceLogger returns a logger for the extra verbose level, which is the level
// reserved for the internal record keeping the design calls for.
//
// The level is not attached to the logger: a slog logger carries attributes, and a
// level belongs to the call. Use Collector.Trace, or pass LevelTrace to Log.
func (c *Collector) TraceLogger(layer Layer, component string) *slog.Logger {
	return c.Logger(layer, component)
}

// Trace records a message at the extra verbose level.
func (c *Collector) Trace(ctx context.Context, layer Layer, component, message string, args ...any) {
	c.Logger(layer, component).Log(ctx, LevelTrace, message, args...)
}

// Enabled reports whether a level is being recorded.
func (c *Collector) Enabled(level slog.Level) bool {
	return level >= c.level.Level()
}

// OTLPEnabled reports whether the bridge is on. It is off unless a caller turns it
// on explicitly, because the design's default is no telemetry at all.
func (c *Collector) OTLPEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.otlpEnabled
}

// EnableOTLP turns the bridge on. It is a separate call rather than a config field
// so that turning telemetry on is always something a caller did on purpose.
func (c *Collector) EnableOTLP(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.otlpEnabled = enabled
}

// enqueue hands a record to the writer goroutine, never blocking.
//
// A full buffer drops the oldest record rather than the caller's turn. Logs are
// for diagnosing, and a log call that can block the work it is describing has its
// priorities backwards.
func (c *Collector) enqueue(record Record) {
	if c.stopped.Load() {
		return
	}
	select {
	case <-c.stop:
		// The collector is shutting down, so a record arriving now is dropped rather
		// than queued for a goroutine that has already finished draining.
		return
	default:
	}
	select {
	case c.records <- record:
	default:
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
	}
}

// Run writes records until the context is cancelled. It blocks, so it is the
// collector's goroutine.
func (c *Collector) Run(ctx context.Context) {
	c.started.Store(true)
	ticker := time.NewTicker(c.config.FlushPeriod)
	defer ticker.Stop()
	defer close(c.done)
	for {
		select {
		case <-ctx.Done():
			// The shutdown flush drains what is already queued, because a log line
			// written just before an orderly shutdown is usually the one explaining
			// it.
			c.drain()
			c.flush()
			return
		case <-c.stop:
			c.drain()
			c.flush()
			return
		case record := <-c.records:
			c.write(record)
		case <-ticker.C:
			c.flush()
		}
	}
}

// drain empties the queue. The receive takes its ok value, so a closed queue ends
// the drain rather than handing back an endless stream of zero records.
func (c *Collector) drain() {
	for {
		select {
		case record, ok := <-c.records:
			if !ok {
				return
			}
			c.write(record)
		default:
			return
		}
	}
}

// Stats reports what the collector has done.
type Stats struct {
	// Written is how many records reached a writer.
	Written uint64 `json:"written"`
	// Dropped is how many were shed because the buffer was full.
	Dropped uint64 `json:"dropped"`
	// ByLevel counts records per level.
	ByLevel map[string]uint64 `json:"by_level"`
	// Pending is how many records are waiting.
	Pending int `json:"pending"`
	// OTLPEnabled reports the bridge state.
	OTLPEnabled bool `json:"otlp_enabled"`
}

// Stats reports the collector's counters.
func (c *Collector) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	byLevel := make(map[string]uint64, len(c.levelCounts))
	for level, count := range c.levelCounts {
		byLevel[level] = count
	}
	return Stats{
		Written:     c.written,
		Dropped:     c.dropped,
		ByLevel:     byLevel,
		Pending:     len(c.records),
		OTLPEnabled: c.otlpEnabled,
	}
}

// Err reports whether records were shed, which is the number a reader needs
// before trusting the log.
func (c *Collector) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped > 0 {
		return fmt.Errorf("%w: %d records were shed", ErrBufferFull, c.dropped)
	}
	return nil
}

// LastRecord returns the most recent record, for a crash report.
func (c *Collector) LastRecord() Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRecord.Clone()
}

// Close stops the collector and waits for its goroutine. Calling it twice is a no
// op, so a shutdown path can call it defensively.
func (c *Collector) Close() {
	c.closeOnce.Do(func() {
		c.stopped.Store(true)
		close(c.stop)
		if c.started.Load() {
			<-c.done
		} else {
			// Nothing is draining, so the queue is emptied here. A collector built
			// and abandoned without ever being run still has to be closable, because
			// a shutdown path cannot know which one it got.
			c.drain()
			c.flush()
			close(c.done)
		}
		c.closeFile()
	})
}

func (c *Collector) closeFile() {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	if c.file == nil {
		return
	}
	_ = c.file.Sync()
	_ = c.file.Close()
	c.file = nil
}

func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: unknown level %q", name)
	}
}
