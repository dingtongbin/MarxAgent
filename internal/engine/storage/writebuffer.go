// SPDX-License-Identifier: Apache-2.0

// Package storage is the durability pipeline. Every record that must survive a
// crash goes through one WriteBuffer, which batches writes, bounds the loss
// window and owns the ordering guarantees the rest of the system relies on.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Record types written to a stream.
const (
	// RecordMessage is one conversation message.
	RecordMessage = "message"
	// RecordEvent is one raw agent event.
	RecordEvent = "event"
	// RecordToolCallState is one tool call state transition.
	RecordToolCallState = "tool_call_state"
	// RecordPartialStream is a partially received model stream.
	RecordPartialStream = "partial_stream"
	// RecordBrokerMessage is a sub agent message.
	RecordBrokerMessage = "broker_msg"
)

// Defaults for the four flush triggers.
const (
	// DefaultWatermarkRecords triggers a write once this many records are queued.
	DefaultWatermarkRecords = 64
	// DefaultWatermarkBytes triggers a write once this many bytes are queued.
	DefaultWatermarkBytes = 256 << 10
	// DefaultFlushPeriod caps the power loss window.
	DefaultFlushPeriod = 500 * time.Millisecond
	// DefaultQueueBytes bounds memory use when a sink stalls.
	DefaultQueueBytes = 64 << 20
)

// ErrBufferClosed reports use after shutdown.
var ErrBufferClosed = errors.New("storage: write buffer is closed")

// ErrQueueOverflow reports that a stalled sink let the queue grow past its bound.
var ErrQueueOverflow = errors.New("storage: journal queue overflow")

// Record is one durable journal entry. Seq is assigned by the buffer and is
// monotonic per stream, which is what recovery and idempotent replay rely on.
type Record struct {
	Stream string          `json:"stream"`
	Seq    int64           `json:"seq"`
	Type   string          `json:"type"`
	Data   json.RawMessage `json:"data"`
	Ts     time.Time       `json:"ts"`
}

// Sink consumes a batch of records. Write must be a single append so a batch
// costs one system call; Sync must make the batch durable.
type Sink interface {
	Write(ctx context.Context, batch []Record) error
	Sync(ctx context.Context) error
}

// Config tunes the flush triggers.
type Config struct {
	// WatermarkRecords triggers a write at this queue depth.
	WatermarkRecords int
	// WatermarkBytes triggers a write at this queued size.
	WatermarkBytes int
	// FlushPeriod caps how long a record waits before it is written and synced.
	FlushPeriod time.Duration
	// QueueBytes bounds the queued bytes; exceeding it drops the oldest records
	// and reports ErrQueueOverflow rather than exhausting memory.
	QueueBytes int
}

func (c Config) withDefaults() Config {
	if c.WatermarkRecords <= 0 {
		c.WatermarkRecords = DefaultWatermarkRecords
	}
	if c.WatermarkBytes <= 0 {
		c.WatermarkBytes = DefaultWatermarkBytes
	}
	if c.FlushPeriod <= 0 {
		c.FlushPeriod = DefaultFlushPeriod
	}
	if c.QueueBytes <= 0 {
		c.QueueBytes = DefaultQueueBytes
	}
	return c
}

// WriteBuffer is the single journaling pipeline for every durable stream.
type WriteBuffer struct {
	config Config
	sinks  []Sink

	mu      sync.Mutex
	pending []Record
	bytes   int
	seq     map[string]int64
	closed  bool
	dropped int

	// writeMu serializes batch writes so Run and Flush cannot interleave.
	writeMu sync.Mutex

	signal chan struct{}
}

// New builds a write buffer over one or more sinks. The first sink is the
// readable journal and the rest are derived stores such as the audit ledger.
func New(config Config, sinks ...Sink) (*WriteBuffer, error) {
	if len(sinks) == 0 {
		return nil, fmt.Errorf("storage: at least one sink is required")
	}
	for index, sink := range sinks {
		if sink == nil {
			return nil, fmt.Errorf("storage: sink %d is nil", index)
		}
	}
	return &WriteBuffer{
		config: config.withDefaults(),
		sinks:  append([]Sink(nil), sinks...),
		seq:    make(map[string]int64),
		signal: make(chan struct{}, 1),
	}, nil
}

// Append queues a record without blocking the caller. The sequence number is
// assigned here so the caller can correlate a record with what reached the sink.
func (b *WriteBuffer) Append(record Record) error {
	if record.Stream == "" {
		return fmt.Errorf("storage: record stream must not be empty")
	}
	if record.Type == "" {
		return fmt.Errorf("storage: record type must not be empty")
	}
	if len(record.Data) == 0 {
		record.Data = json.RawMessage(`null`)
	}
	if !json.Valid(record.Data) {
		return fmt.Errorf("storage: record data must be valid JSON")
	}
	if record.Ts.IsZero() {
		record.Ts = time.Now().UTC()
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBufferClosed
	}
	b.seq[record.Stream]++
	record.Seq = b.seq[record.Stream]
	b.pending = append(b.pending, record)
	b.bytes += len(record.Data) + len(record.Stream) + len(record.Type) + 64
	overflow := b.bytes > b.config.QueueBytes
	if overflow {
		b.dropOverflowLocked()
	}
	full := len(b.pending) >= b.config.WatermarkRecords || b.bytes >= b.config.WatermarkBytes
	b.mu.Unlock()

	if full || overflow {
		b.wake()
	}
	if overflow {
		return ErrQueueOverflow
	}
	return nil
}

// LastSeq reports the sequence number assigned to a stream.
func (b *WriteBuffer) LastSeq(stream string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq[stream]
}

// Dropped reports how many records were discarded to respect the queue bound.
func (b *WriteBuffer) Dropped() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Queued reports how many records are waiting to be written.
func (b *WriteBuffer) Queued() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

// Flush writes and syncs everything queued. It is the boundary trigger, used
// after a loop ends, after a tool call settles and during shutdown.
func (b *WriteBuffer) Flush(ctx context.Context) error {
	if ctx == nil {
		return ErrNilContext
	}
	return b.drain(ctx, true)
}

// Run drives the periodic trigger until the context is canceled, then performs
// the fallback flush so a normal shutdown loses nothing.
func (b *WriteBuffer) Run(ctx context.Context) error {
	if ctx == nil {
		return ErrNilContext
	}
	ticker := time.NewTicker(b.config.FlushPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// The shutdown budget belongs to the caller, so the fallback flush
			// uses a detached context with its own bound.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.config.FlushPeriod*10)
			err := b.drainAll(flushCtx, true)
			cancel()
			b.markClosed()
			return err
		case <-ticker.C:
			// The periodic trigger is what caps the power loss window, so it both
			// writes and syncs; the watermark trigger only writes.
			if err := b.drainAll(ctx, true); err != nil {
				return err
			}
		case <-b.signal:
			if err := b.drainAll(ctx, false); err != nil {
				return err
			}
		}
	}
}

// drainAll writes until the queue is empty.
//
// A single drain is not enough, and the reason is a lost wakeup rather than a
// slow sink. A drain takes the queue and then releases the lock to write, so a
// record appended during that write reaches the watermark and sets the signal
// while the signal this drain was started by is still being handled. The set is
// dropped when a token is already pending, and the pending token is then consumed
// by the drain already in progress, which had snapshotted the queue before the
// record arrived. The record is left with nothing to wake it and, if the flush
// period is long, nothing to save it either.
//
// Draining to empty makes the queue rather than the wakeup the thing that decides
// when writing stops, so a wakeup is only ever a hint that something may be
// waiting rather than the last chance to notice it.
func (b *WriteBuffer) drainAll(ctx context.Context, sync bool) error {
	for {
		if err := b.drain(ctx, false); err != nil {
			return err
		}
		if b.Queued() == 0 {
			break
		}
	}
	if sync {
		// The sink is synced once for the whole run rather than once per batch,
		// because the records are already written and a sync per batch would cost a
		// call per watermark for no additional durability.
		b.syncSinks(ctx)
	}
	return nil
}

// Close stops accepting records and performs the fallback flush.
func (b *WriteBuffer) Close(ctx context.Context) error {
	if ctx == nil {
		return ErrNilContext
	}
	// The queue is drained to empty for the same reason the loop drains to empty:
	// a caller closing the buffer is promising that nothing is left in memory, and
	// a record that arrived during the final write would otherwise break it.
	err := b.drainAll(ctx, true)
	b.markClosed()
	return err
}

// ErrNilContext reports a missing context.
var ErrNilContext = errors.New("storage: context must not be nil")

func (b *WriteBuffer) markClosed() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
}

func (b *WriteBuffer) wake() {
	select {
	case b.signal <- struct{}{}:
	default:
	}
}

// dropOverflowLocked discards the oldest records, because a stalled sink must
// not cost the newest records their place in the queue.
func (b *WriteBuffer) dropOverflowLocked() {
	limit := b.config.QueueBytes / 2
	for b.bytes > limit && len(b.pending) > 1 {
		dropped := b.pending[0]
		b.pending = b.pending[1:]
		b.bytes -= len(dropped.Data) + len(dropped.Stream) + len(dropped.Type) + 64
		b.dropped++
	}
}

// drain moves the queue into the sinks. The queue swap is cheap and happens
// under the queue lock; the write itself is serialized separately so a caller
// waiting in Flush cannot interleave with the writer goroutine.
func (b *WriteBuffer) drain(ctx context.Context, sync bool) error {
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		if sync {
			b.syncSinks(ctx)
		}
		return nil
	}
	batch := b.pending
	b.pending = nil
	b.bytes = 0
	b.mu.Unlock()

	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	for _, sink := range b.sinks {
		if err := sink.Write(ctx, batch); err != nil {
			return fmt.Errorf("storage: sink write: %w", err)
		}
	}
	if sync {
		return b.syncSinksLocked(ctx)
	}
	return nil
}

func (b *WriteBuffer) syncSinks(ctx context.Context) {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_ = b.syncSinksLocked(ctx)
}

func (b *WriteBuffer) syncSinksLocked(ctx context.Context) error {
	for _, sink := range b.sinks {
		if err := sink.Sync(ctx); err != nil {
			return fmt.Errorf("storage: sink sync: %w", err)
		}
	}
	return nil
}
