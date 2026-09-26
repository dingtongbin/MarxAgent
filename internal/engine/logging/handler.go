// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// collectorHandler adapts slog to the collector.
//
// It is a handler rather than a call into slog's own writers because the design
// wants the frozen field set assembled in one place: slog's own JSON handler
// cannot be made to guarantee that every record carries every field, and a field
// that is sometimes missing is a query that sometimes misses.
type collectorHandler struct {
	collector *Collector
	layer     Layer
	component string
	attrs     []slog.Attr
	groups    []string
}

// Enabled reports whether the collector is recording this level.
func (h *collectorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.collector.Enabled(level)
}

// Handle turns one record into the frozen field set.
func (h *collectorHandler) Handle(_ context.Context, record slog.Record) error {
	fields := make(map[string]any, len(FrozenFields)+record.NumAttrs()+len(h.attrs))
	// The frozen set comes first and is never overwritten by a caller's attribute of
	// the same name. A caller that logs FieldLevel itself is trying to forge a
	// record, and the design's auditability depends on those fields meaning what
	// they say.
	fields[FieldTimestamp] = h.collector.config.Clock().Format(time.RFC3339Nano)
	fields[FieldLevel] = levelName(record.Level)
	fields[FieldMessage] = record.Message
	fields[FieldService] = ServiceName
	fields[FieldLayer] = string(h.layer)
	fields[FieldComponent] = h.component
	fields[FieldSessionID] = ""
	fields[FieldAgentID] = ""
	fields[FieldTraceID] = ""
	fields[FieldSpanID] = ""
	fields[FieldEventType] = ""
	fields[FieldError] = ""

	for _, attr := range h.attrs {
		assign(fields, h.groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		assign(fields, h.groups, attr)
		return true
	})

	entry := Record{Fields: fields, At: h.collector.config.Clock()}
	h.collector.record(entry)
	return nil
}

// WithAttrs returns a handler carrying extra attributes.
func (h *collectorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kept := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		if isReserved(attr.Key) {
			// A reserved attribute is dropped rather than passed on, so the frozen set
			// cannot be widened by a caller who happens to use the same name.
			continue
		}
		kept = append(kept, attr)
	}
	if len(kept) == 0 {
		return h
	}
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), kept...)
	return &clone
}

// WithGroup returns a handler nesting attributes under a name.
func (h *collectorHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func assign(fields map[string]any, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	// A group of attributes is expanded rather than nested, because the field set
	// is flat and a reader querying a nested object needs a parser.
	if attr.Value.Kind() == slog.KindGroup {
		attributes := attr.Value.Group()
		if len(groups) > 0 || attr.Key != "" {
			prefix := strings.Join(append(append([]string(nil), groups...), attr.Key), ".")
			for _, nested := range attributes {
				assignPrefixed(fields, prefix, nested)
			}
			return
		}
		for _, nested := range attributes {
			assign(fields, groups, nested)
		}
		return
	}
	if len(groups) > 0 {
		assignPrefixed(fields, strings.Join(groups, "."), attr)
		return
	}
	// A reserved field is never assigned. The frozen set comes from the record's own
	// facts, and a caller that logs one of these names is trying to forge a record,
	// which the design's auditability cannot allow.
	if isReserved(attr.Key) {
		return
	}
	fields[attr.Key] = attr.Value.Any()
}

func assignPrefixed(fields map[string]any, prefix string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindGroup {
		for _, nested := range attr.Value.Group() {
			assignPrefixed(fields, prefix+"."+nested.Key, nested)
		}
		return
	}
	fields[prefix+"."+attr.Key] = attr.Value.Any()
}

func isReserved(key string) bool {
	for _, reserved := range FrozenFields {
		if key == reserved {
			return true
		}
	}
	return false
}

func levelName(level slog.Level) string {
	switch {
	case level < slog.LevelDebug:
		return "trace"
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	default:
		return "error"
	}
}

// record notes a record and hands it to the writer goroutine.
func (c *Collector) record(record Record) {
	c.mu.Lock()
	c.lastRecord = record
	c.levelCounts[levelNameOf(record)]++
	c.mu.Unlock()
	c.enqueue(record)
}

func levelNameOf(record Record) string {
	if value, ok := record.Field(FieldLevel); ok {
		if name, isString := value.(string); isString {
			return name
		}
	}
	return "info"
}

// write renders and writes one record. Every failure is swallowed on purpose: a
// log that cannot be written is not a reason to fail the work it describes.
func (c *Collector) write(record Record) {
	rendered, err := c.render(record)
	if err != nil {
		return
	}
	if err := c.emit(rendered, record.At); err != nil {
		return
	}
	c.mu.Lock()
	c.written++
	c.mu.Unlock()
}

func (c *Collector) render(record Record) ([]byte, error) {
	if c.config.Format == "text" {
		var builder strings.Builder
		for _, field := range FrozenFields {
			value, present := record.Field(field)
			if !present || value == nil || value == "" {
				continue
			}
			writeTextField(&builder, field, value)
		}
		for key, value := range record.Fields {
			if isReserved(key) {
				continue
			}
			writeTextField(&builder, key, value)
		}
		return []byte(strings.TrimSpace(builder.String()) + "\n"), nil
	}
	// The frozen fields are written in their declared order, then everything else
	// sorted, so two runs of the same code produce the same bytes and a diff of two
	// logs is readable.
	ordered := make(map[string]any, len(record.Fields))
	for _, field := range FrozenFields {
		if value, ok := record.Field(field); ok {
			ordered[field] = value
		}
	}
	extra := make([]string, 0, len(record.Fields))
	for key := range record.Fields {
		if isReserved(key) {
			continue
		}
		extra = append(extra, key)
	}
	sortStrings(extra)
	for _, key := range extra {
		ordered[key] = record.Fields[key]
	}
	encoded, err := json.Marshal(ordered)
	if err != nil {
		return nil, fmt.Errorf("logging: encode the record: %w", err)
	}
	return append(encoded, '\n'), nil
}

// writeTextField writes one field of the text format, quoting a value that would
// otherwise make the line impossible to split on whitespace.
func writeTextField(builder *strings.Builder, key string, value any) {
	rendered := fmt.Sprint(value)
	if strings.ContainsAny(rendered, " \t=\"\n") {
		rendered = strconv.Quote(rendered)
	}
	builder.WriteString(key)
	builder.WriteByte('=')
	builder.WriteString(rendered)
	builder.WriteByte(' ')
}

func sortStrings(values []string) {
	for outer := 1; outer < len(values); outer++ {
		for inner := outer; inner > 0 && values[inner] < values[inner-1]; inner-- {
			values[inner], values[inner-1] = values[inner-1], values[inner]
		}
	}
}

// emit writes the rendered bytes to the external writer or the current file.
func (c *Collector) emit(rendered []byte, at time.Time) error {
	if c.external != nil {
		_, err := c.external.Write(rendered)
		return err
	}
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	// The record's own timestamp chooses the file. Using the wall clock here would
	// put a record in the wrong day's file whenever the collector's clock is not
	// the wall clock, which is exactly when a reader would be looking for it.
	file, err := c.fileFor(at)
	if err != nil {
		return err
	}
	if _, err := file.Write(rendered); err != nil {
		return err
	}
	return nil
}

// flush forces a sync, which the shutdown path needs and the periodic tick does
// not: the tick bounds the loss window, and a sync is what closes it.
func (c *Collector) flush() {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	if c.file == nil {
		return
	}
	_ = c.file.Sync()
}

// fileFor returns the file for a day, rolling over when the date or the size says
// to.
func (c *Collector) fileFor(at time.Time) (*os.File, error) {
	day := at.Format("2006-01-02")
	if c.file != nil && c.day == day && !c.rollNeeded() {
		return c.file, nil
	}
	if c.file != nil {
		_ = c.file.Sync()
		_ = c.file.Close()
		c.file = nil
	}
	path := filepath.Join(c.config.Directory, fmt.Sprintf("%s-%s.json", ServiceName, day))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("logging: open the log file: %w", err)
	}
	c.file = file
	c.day = day
	return file, nil
}

// rollNeeded reports whether the current file is over its size limit.
func (c *Collector) rollNeeded() bool {
	if c.config.MaxFileBytes <= 0 || c.file == nil {
		return false
	}
	info, err := c.file.Stat()
	if err != nil {
		return false
	}
	return info.Size() >= c.config.MaxFileBytes
}

// FilePath reports the file records are being written to, which is what a support
// question needs.
func (c *Collector) FilePath() string {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	if c.file == nil {
		return ""
	}
	return c.file.Name()
}

// CountingWriter counts what was written and can be made to fail, so a test can
// prove that a broken log never reaches the caller.
type CountingWriter struct {
	mu       sync.Mutex
	written  int
	FailWith error
	// Sink receives the bytes when set.
	Sink func([]byte)
}

// Write implements io.Writer.
func (w *CountingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	if w.FailWith != nil {
		err := w.FailWith
		w.mu.Unlock()
		return 0, err
	}
	w.written += len(data)
	sink := w.Sink
	w.mu.Unlock()
	if sink != nil {
		sink(append([]byte(nil), data...))
	}
	return len(data), nil
}

// Count reports how many bytes were written.
func (w *CountingWriter) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// Fail makes every write fail.
func (w *CountingWriter) Fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.FailWith = err
}

// start runs a collector in the background and returns a function that stops it.
func start(t interface {
	Cleanup(func())
	Helper()
}, collector *Collector) *Collector {
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		collector.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
		collector.Close()
	})
	return collector
}

// parseFileDay reads the day out of a log file name, which is how a test asserts
// the rolling happened.
// parseFileDay reads the date out of a log file name, which is how a test asserts
// that records landed in the day the collector's clock says they should.
func parseFileDay(name string) string {
	base := strings.TrimSuffix(filepath.Base(name), ".json")
	parts := strings.Split(base, "-")
	// The name is marxagent-YYYY-MM-DD, so the date is the last three parts and
	// each of them has to be a number.
	if len(parts) < 4 {
		return ""
	}
	year, month, day := parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1]
	for _, part := range []string{year, month, day} {
		if _, err := strconv.Atoi(part); err != nil {
			return ""
		}
	}
	return year + "-" + month + "-" + day
}
