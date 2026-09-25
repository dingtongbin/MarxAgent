// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stagedDoneContext struct {
	calls     atomic.Int32
	threshold int32
	done      chan struct{}
	once      sync.Once
}

func newStagedDoneContext(threshold int32) *stagedDoneContext {
	return &stagedDoneContext{threshold: threshold, done: make(chan struct{})}
}

func (ctx *stagedDoneContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (ctx *stagedDoneContext) Done() <-chan struct{} {
	if ctx.calls.Add(1) >= ctx.threshold {
		ctx.once.Do(func() { close(ctx.done) })
	}
	return ctx.done
}

func (ctx *stagedDoneContext) Err() error {
	select {
	case <-ctx.done:
		return context.Canceled
	default:
		return nil
	}
}

func (ctx *stagedDoneContext) Value(any) any {
	return nil
}

func TestEventSubscriptionNextCancelsWhileWaiting(t *testing.T) {
	for _, threshold := range []int32{1, 2, 3} {
		t.Run(fmt.Sprintf("stage-%d", threshold), func(t *testing.T) {
			ctx := newStagedDoneContext(threshold)
			subscriber := &eventSubscription{ctx: ctx, wake: make(chan struct{}, 1)}
			if _, open := subscriber.next(); open {
				t.Fatal("next() returned an event after cancellation")
			}
		})
	}
}

func TestEventBusFiltersPreservesOrderAndCopiesPublishData(t *testing.T) {
	bus := &asyncEventBus{}
	received := make(chan Event, 3)
	contextValue := &struct{}{}
	unsubscribe := bus.Subscribe(EventTypeAll, func(ctx context.Context, event Event) {
		if value := ctx.Value(contextValue); value != contextValue {
			t.Errorf("delivery context value = %v, want original value", value)
		}
		if deadline, ok := ctx.Deadline(); ok || !deadline.IsZero() {
			t.Errorf("delivery deadline = %v, %v; want none", deadline, ok)
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("delivery context error = %v", err)
		}
		received <- event
	})
	defer unsubscribe()
	filteredEvents := make(chan Event, 2)
	filteredUnsubscribe := bus.Subscribe(EventTypeError, func(_ context.Context, event Event) {
		filteredEvents <- event
	})
	defer filteredUnsubscribe()

	bus.Publish(context.WithValue(context.Background(), contextValue, contextValue), Event{
		Type: EventTypeDone,
		Data: `{"sequence":1}`,
	})
	first := waitForEvent(t, received)
	if first.Type != EventTypeDone || first.Data != `{"sequence":1}` {
		t.Fatalf("filtered event = %#v", first)
	}
	select {
	case event := <-filteredEvents:
		t.Fatalf("error-only subscriber received %s", event.Type)
	default:
	}
	valueContext := context.WithValue(context.Background(), contextValue, contextValue)
	bus.Publish(valueContext, Event{Type: EventTypeError, Data: `{"sequence":2}`})
	second := waitForEvent(t, received)
	bus.Publish(valueContext, Event{Type: EventTypeError, Data: `{"sequence":3}`})
	third := waitForEvent(t, received)
	if second.Data != `{"sequence":2}` || third.Data != `{"sequence":3}` {
		t.Fatalf("delivery order = %s, %s", second.Data, third.Data)
	}
}

func TestEventBusGivesEachSubscriberIndependentImmutableData(t *testing.T) {
	bus := &asyncEventBus{}
	firstMutated := make(chan struct{})
	firstDone := make(chan struct{})
	secondChecked := make(chan struct{})
	firstUnsubscribe := bus.Subscribe(EventTypeAll, func(_ context.Context, event Event) {
		event.Data = "mutated"
		close(firstMutated)
		close(firstDone)
	})
	defer firstUnsubscribe()
	secondUnsubscribe := bus.Subscribe(EventTypeAll, func(_ context.Context, event Event) {
		<-firstMutated
		if event.Data != `{"value":1}` {
			t.Errorf("second subscriber observed mutation: %s", event.Data)
		}
		close(secondChecked)
	})
	defer secondUnsubscribe()

	bus.Publish(context.Background(), Event{Type: EventTypeDone, Data: `{"value":1}`})
	waitForSignal(t, firstDone)
	waitForSignal(t, secondChecked)
}

func TestEventBusUnsubscribeStopsDeliveryAndIsIdempotent(t *testing.T) {
	bus := &asyncEventBus{}
	received := make(chan Event, 2)
	unsubscribe := bus.Subscribe(EventTypeDone, func(_ context.Context, event Event) {
		received <- event
	})
	bus.Publish(context.Background(), Event{Type: EventTypeDone})
	waitForEvent(t, received)
	unsubscribe()
	unsubscribe()
	bus.Publish(context.Background(), Event{Type: EventTypeDone})
	select {
	case event := <-received:
		t.Fatalf("received event after unsubscribe: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestEventBusHandlesNilHandlerAndIsolatesHandlerPanic(t *testing.T) {
	bus := &asyncEventBus{}
	nilUnsubscribe := bus.Subscribe(EventTypeDone, nil)
	panickingUnsubscribe := bus.Subscribe(EventTypeDone, func(context.Context, Event) {
		panic("handler panic")
	})
	defer panickingUnsubscribe()
	var delivered atomic.Int32
	healthyUnsubscribe := bus.Subscribe(EventTypeDone, func(context.Context, Event) {
		delivered.Add(1)
	})
	defer healthyUnsubscribe()

	bus.Publish(context.Background(), Event{Type: EventTypeDone})
	bus.Publish(context.Background(), Event{Type: EventTypeDone})
	deadline := time.Now().Add(time.Second)
	for delivered.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if delivered.Load() != 2 {
		t.Fatalf("healthy subscriber received %d events, want 2", delivered.Load())
	}
	nilUnsubscribe()
}

func TestEventBusCancellationIsVisibleToActiveDelivery(t *testing.T) {
	bus := &asyncEventBus{}
	contextCapture := make(chan context.Context, 1)
	unsubscribe := bus.Subscribe(EventTypeDone, func(ctx context.Context, _ Event) {
		contextCapture <- ctx
	})
	bus.Publish(nil, Event{Type: EventTypeDone})
	deliveryContext := waitForContext(t, contextCapture)
	unsubscribe()
	deadline := time.Now().Add(time.Second)
	for deliveryContext.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !errorsIsContextCanceled(deliveryContext.Err()) {
		t.Fatalf("delivery context error = %v, want context canceled", deliveryContext.Err())
	}
	select {
	case <-deliveryContext.Done():
	default:
		t.Fatal("delivery context Done channel is not closed")
	}
}

func TestEventBusConcurrentPublishSubscribeAndUnsubscribe(t *testing.T) {
	bus := &asyncEventBus{}
	var waitGroup sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for iteration := 0; iteration < 100; iteration++ {
				unsubscribe := bus.Subscribe(EventTypeAll, func(context.Context, Event) {})
				bus.Publish(context.Background(), Event{Type: EventTypeDone, Data: `{}`})
				unsubscribe()
			}
		}()
	}
	waitGroup.Wait()
}

func TestNewEventMarshalsPayloadAndRejectsUnsupportedData(t *testing.T) {
	event, err := newEvent(EventTypeCompact, map[string]string{"reason": "threshold"})
	if err != nil {
		t.Fatalf("newEvent() error = %v", err)
	}
	if event.Type != EventTypeCompact || event.Data != `{"reason":"threshold"}` {
		t.Fatalf("newEvent() = %#v", event)
	}
	dataBytes := event.DataBytes()
	dataBytes[0] = 'X'
	if event.Data != `{"reason":"threshold"}` {
		t.Fatal("DataBytes() exposed mutable event storage")
	}
	if event.Timestamp.IsZero() {
		t.Fatal("newEvent() timestamp is zero")
	}
	if _, err := newEvent(EventTypeError, make(chan struct{})); err == nil {
		t.Fatal("newEvent() accepted unsupported payload")
	}
}

func waitForEvent(t testing.TB, events <-chan Event) Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func waitForContext(t testing.TB, contexts <-chan context.Context) context.Context {
	t.Helper()
	select {
	case value := <-contexts:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for context")
		return nil
	}
}

func waitForSignal(t testing.TB, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func errorsIsContextCanceled(err error) bool {
	return err == context.Canceled
}
