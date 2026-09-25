// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const (
	// EventTypeStream carries a completed assistant stream.
	EventTypeStream EventType = "stream"
	// EventTypeToolCall carries accepted tool calls.
	EventTypeToolCall EventType = "tool_call"
	// EventTypeToolResult carries settled tool results.
	EventTypeToolResult EventType = "tool_result"
	// EventTypeStateChange carries an appended state record.
	EventTypeStateChange EventType = "state_change"
	// EventTypeError carries a non-terminal or terminal error.
	EventTypeError EventType = "error"
	// EventTypeDone carries successful loop completion.
	EventTypeDone EventType = "done"
	// EventTypeSubAgentMessage carries an in-process sub-agent message.
	EventTypeSubAgentMessage EventType = "sub_agent_msg"
	// EventTypeCompact carries a context compaction notification.
	EventTypeCompact EventType = "compact"
	// EventTypePartial carries an incremental provider chunk.
	EventTypePartial EventType = "partial"
	// EventTypeAll subscribes an observer to every event type.
	EventTypeAll EventType = ""
)

// EventType identifies an immutable event-bus record.
type EventType string

// Event is the immutable durability and UI source emitted by the core.
type Event struct {
	Type      EventType `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"session_id,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	TraceID   string    `json:"trace_id,omitempty"`
	SpanID    string    `json:"span_id,omitempty"`
	Data      string    `json:"data,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// DataBytes returns an owned JSON byte slice for durable consumers.
func (event Event) DataBytes() []byte {
	return []byte(event.Data)
}

// EventHandler receives one immutable event.
type EventHandler func(ctx context.Context, event Event)

// EventBus asynchronously fans out events without blocking publishers.
type EventBus interface {
	Publish(ctx context.Context, event Event)
	Subscribe(eventType EventType, handler EventHandler) func()
}

// StreamPayload carries an incremental or completed provider chunk.
type StreamPayload struct {
	MessageID string      `json:"message_id"`
	Chunk     StreamChunk `json:"chunk"`
}

// ToolCallPayload carries the accepted calls from one assistant message.
type ToolCallPayload struct {
	MessageID string         `json:"message_id"`
	Calls     []ContentBlock `json:"calls"`
}

// ToolResultPayload carries settled results in original call order.
type ToolResultPayload struct {
	Results []ToolResult `json:"results"`
}

// StateChangePayload carries one immutable state transition.
type StateChangePayload struct {
	Reason  string  `json:"reason"`
	Message Message `json:"message"`
}

// ErrorPayload carries a stable operation label and error text.
type ErrorPayload struct {
	Operation string `json:"operation"`
	Message   string `json:"message"`
}

// DonePayload carries the final read-only state snapshot.
type DonePayload struct {
	State StateSnapshot `json:"state"`
}

type asyncEventBus struct {
	mu             sync.Mutex
	subscribers    []*eventSubscription
	incoming       []eventDelivery
	wake           chan struct{}
	dispatchCancel context.CancelFunc
	workers        sync.WaitGroup
}

type eventSubscription struct {
	ctx       context.Context
	cancel    context.CancelFunc
	bus       *asyncEventBus
	eventType EventType
	handler   EventHandler
	mu        sync.Mutex
	queue     []eventDelivery
	head      int
	wake      chan struct{}
}

type eventDelivery struct {
	ctx   context.Context
	event Event
}

type deliveryContext struct {
	values context.Context
	done   <-chan struct{}
}

func (c deliveryContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (c deliveryContext) Done() <-chan struct{} {
	return c.done
}

func (c deliveryContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c deliveryContext) Value(key any) any {
	return c.values.Value(key)
}

func (b *asyncEventBus) Publish(ctx context.Context, event Event) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	if len(b.subscribers) == 0 {
		b.mu.Unlock()
		return
	}
	b.ensureDispatcherLocked()
	b.incoming = append(b.incoming, eventDelivery{ctx: ctx, event: event})
	select {
	case b.wake <- struct{}{}:
	default:
	}
	b.mu.Unlock()
}

func (b *asyncEventBus) Subscribe(eventType EventType, handler EventHandler) func() {
	ctx, cancel := context.WithCancel(context.Background())
	subscriber := &eventSubscription{
		ctx:       ctx,
		cancel:    cancel,
		bus:       b,
		eventType: eventType,
		handler:   handler,
		queue:     make([]eventDelivery, 0, 256),
		wake:      make(chan struct{}, 1),
	}

	b.mu.Lock()
	b.subscribers = append(b.subscribers, subscriber)
	b.ensureDispatcherLocked()
	b.mu.Unlock()

	b.workers.Add(1)
	go subscriber.run()
	return func() {
		subscriber.unsubscribe()
	}
}

func (b *asyncEventBus) ensureDispatcherLocked() {
	if b.dispatchCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	b.dispatchCancel = cancel
	b.wake = wake
	b.workers.Add(1)
	go func() {
		defer b.workers.Done()
		b.runDispatcher(ctx, wake)
	}()
}

func (b *asyncEventBus) runDispatcher(ctx context.Context, wake <-chan struct{}) {
	for {
		b.mu.Lock()
		select {
		case <-ctx.Done():
			b.mu.Unlock()
			return
		default:
		}
		if len(b.incoming) > 0 {
			delivery := b.incoming[0]
			b.incoming[0] = eventDelivery{}
			b.incoming = b.incoming[1:]
			if len(b.incoming) == 0 {
				b.incoming = nil
			}
			subscribers := append([]*eventSubscription(nil), b.subscribers...)
			b.mu.Unlock()
			for _, subscriber := range subscribers {
				if subscriber.eventType != EventTypeAll && subscriber.eventType != delivery.event.Type {
					continue
				}
				subscriber.enqueue(eventDelivery{
					ctx:   deliveryContext{values: delivery.ctx, done: subscriber.ctx.Done()},
					event: delivery.event,
				})
			}
			continue
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
	}
}

func (s *eventSubscription) enqueue(delivery eventDelivery) {
	s.mu.Lock()
	select {
	case <-s.ctx.Done():
		s.mu.Unlock()
		return
	default:
	}
	wasEmpty := s.head == len(s.queue)
	s.queue = append(s.queue, delivery)
	if wasEmpty {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *eventSubscription) next() (eventDelivery, bool) {
	for {
		select {
		case <-s.ctx.Done():
			return eventDelivery{}, false
		default:
		}

		s.mu.Lock()
		select {
		case <-s.ctx.Done():
			s.mu.Unlock()
			return eventDelivery{}, false
		default:
		}
		if s.head < len(s.queue) {
			delivery := s.queue[s.head]
			s.queue[s.head] = eventDelivery{}
			s.head++
			if s.head == len(s.queue) {
				s.queue = s.queue[:0]
				s.head = 0
			}
			s.mu.Unlock()
			return delivery, true
		}
		s.mu.Unlock()

		select {
		case <-s.ctx.Done():
			return eventDelivery{}, false
		case <-s.wake:
		}
	}
}

func (s *eventSubscription) run() {
	defer s.bus.workers.Done()
	for {
		delivery, ok := s.next()
		if !ok {
			return
		}
		s.deliver(delivery)
	}
}

func (s *eventSubscription) deliver(delivery eventDelivery) {
	defer func() {
		_ = recover()
	}()
	if s.handler == nil {
		return
	}
	s.handler(delivery.ctx, delivery.event)
}

func (s *eventSubscription) unsubscribe() {
	s.bus.mu.Lock()
	removed := false
	for index, subscriber := range s.bus.subscribers {
		if subscriber == s {
			s.bus.subscribers = append(s.bus.subscribers[:index], s.bus.subscribers[index+1:]...)
			removed = true
			break
		}
	}
	if removed && len(s.bus.subscribers) == 0 && s.bus.dispatchCancel != nil {
		s.bus.dispatchCancel()
		s.bus.dispatchCancel = nil
		s.bus.incoming = nil
		s.bus.wake = nil
	}
	s.bus.mu.Unlock()
	s.cancel()
}

var marshalEventData = json.Marshal

func newEvent(eventType EventType, data any) (Event, error) {
	encoded, err := marshalEventData(data)
	if err != nil {
		return Event{}, fmt.Errorf("core: marshal %s event: %w", eventType, err)
	}
	return Event{Type: eventType, Timestamp: time.Now().UTC(), Data: string(encoded)}, nil
}
