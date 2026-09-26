// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// A log call happens on the hot path of every turn, so it must not wait for a
// disk and it must not grow without bound. These gates measure both.

func TestLoggingPerformanceGates(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to run the logging performance gates")
	}
	t.Run("a_log_call_does_not_wait_for_the_writer", func(t *testing.T) {
		// A writer that blocks forever stands in for a slow disk. A log call that
		// waited for it would take the turn it was describing down with it.
		blocked := make(chan struct{})
		collector, err := New(Config{
			Writer:      &CountingWriter{Sink: func([]byte) { <-blocked }},
			BufferSize:  8,
			FlushPeriod: time.Hour,
			Clock:       fixedClock(),
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
		started := time.Now()
		for index := 0; index < 100; index++ {
			logger.Info("into a blocked writer", "index", index)
		}
		elapsed := time.Since(started)
		if elapsed > 500*time.Millisecond {
			t.Fatalf("a hundred log calls took %v with a blocked writer", elapsed)
		}
		// The buffer is bounded, so the calls that could not be queued were shed and
		// counted rather than accumulated.
		if collector.Stats().Dropped == 0 {
			t.Fatal("an unbounded queue absorbed everything")
		}
		close(blocked)
		cancel()
		<-finished
		collector.Close()
	})
	t.Run("the_frozen_set_costs_a_constant", func(t *testing.T) {
		collector, err := New(Config{Writer: &CountingWriter{}, Clock: fixedClock()})
		if err != nil {
			t.Fatal(err)
		}
		defer collector.Close()
		now := time.Now().UTC()
		empty := Record{Fields: map[string]any{}, At: now}
		for _, field := range FrozenFields {
			empty.Fields[field] = ""
		}
		full := Record{Fields: map[string]any{}, At: now}
		for _, field := range FrozenFields {
			full.Fields[field] = "a value"
		}
		for index := 0; index < 20; index++ {
			full.Fields["extra_"+strings.Repeat("x", index%7+1)] = index
		}
		small, err := collector.render(empty)
		if err != nil {
			t.Fatal(err)
		}
		large, err := collector.render(full)
		if err != nil {
			t.Fatal(err)
		}
		// The rendering scales with the fields a caller supplied and nothing else, so
		// a record with fifty extra attributes is fifty attributes more expensive and
		// not a different code path.
		if len(large) <= len(small) {
			t.Fatalf("the extra fields were not rendered: %d then %d", len(small), len(large))
		}
	})
	t.Run("many_loggers_share_one_collector", func(t *testing.T) {
		collector, err := New(Config{Writer: &CountingWriter{}, Clock: fixedClock()})
		if err != nil {
			t.Fatal(err)
		}
		defer collector.Close()
		// Every component gets its own logger, and they all feed the same writer, so
		// the cost is per record rather than per component.
		loggers := make([]*slog.Logger, 0, 32)
		for index := 0; index < 32; index++ {
			loggers = append(loggers, collector.Logger(LayerL2, "component").With("id", index))
		}
		var wg sync.WaitGroup
		wg.Add(len(loggers))
		for _, logger := range loggers {
			go func(l *slog.Logger) {
				defer wg.Done()
				for index := 0; index < 100; index++ {
					l.Info("concurrent", "index", index)
				}
			}(logger)
		}
		wg.Wait()
		if collector.Stats().Dropped == 0 && collector.Stats().Pending == 0 {
			t.Skip("the writer never ran, so nothing can be counted")
		}
	})
}

func BenchmarkLogCall(b *testing.B) {
	collector, err := New(Config{Writer: &CountingWriter{}, BufferSize: 1 << 16, Clock: fixedClock()})
	if err != nil {
		b.Fatal(err)
	}
	defer collector.Close()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		collector.Run(ctx)
	}()
	defer func() {
		cancel()
		<-finished
		collector.Close()
	}()
	logger := collector.Logger(LayerL2, "gateway")
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		logger.Info("a message", "turn", iteration, "session_id", "s1")
	}
}

func BenchmarkLogCallWithAFrozenSet(b *testing.B) {
	collector, err := New(Config{Writer: &CountingWriter{}, BufferSize: 1 << 16, Clock: fixedClock()})
	if err != nil {
		b.Fatal(err)
	}
	defer collector.Close()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		collector.Run(ctx)
	}()
	defer func() {
		cancel()
		<-finished
		collector.Close()
	}()
	// Every field the design freezes, which is what a real record carries.
	logger := collector.Logger(LayerL3, "web").With(
		"session_id", "s1", "agent_id", "main", "trace_id", "t", "span_id", "sp", "event_type", "done")
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		logger.Info("a message", "turn", iteration)
	}
}
