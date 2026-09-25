// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestHookRegistryValidatesRegistration(t *testing.T) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session", "agent")
	hook := func(context.Context, any) (any, error) { return nil, nil }
	tests := []struct {
		name     string
		hookType HookType
		priority int
		hook     HookFunc
		wantErr  error
	}{
		{name: "invalid type", hookType: "unknown", priority: 0, hook: hook, wantErr: ErrInvalidHookType},
		{name: "priority low", hookType: HookPreLoop, priority: -101, hook: hook, wantErr: ErrInvalidHookPriority},
		{name: "priority high", hookType: HookPreLoop, priority: 101, hook: hook, wantErr: ErrInvalidHookPriority},
		{name: "nil hook", hookType: HookPreLoop, priority: 0, hook: nil, wantErr: ErrNilHook},
		{name: "minimum priority", hookType: HookPreLoop, priority: -100, hook: hook},
		{name: "maximum priority", hookType: HookPostLoop, priority: 100, hook: hook},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := registry.Register(test.hookType, test.priority, test.hook)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Register() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestHookRegistryOrdersAndChainsHooks(t *testing.T) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session", "agent")
	var mu sync.Mutex
	order := make([]string, 0, 3)
	register := func(name string, priority int) {
		t.Helper()
		hook := func(_ context.Context, data any) (any, error) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return data.(string) + name, nil
		}
		if err := registry.Register(HookPreLoop, priority, hook); err != nil {
			t.Fatalf("Register(%s) error = %v", name, err)
		}
	}
	register("late", 20)
	register("first-equal", 0)
	register("early", -20)
	register("second-equal", 0)

	value, err := registry.trigger(context.Background(), HookPreLoop, "value:")
	if err != nil {
		t.Fatalf("trigger() error = %v", err)
	}
	if value != "value:earlyfirst-equalsecond-equallate" {
		t.Fatalf("chained value = %v", value)
	}
	if !reflect.DeepEqual(order, []string{"early", "first-equal", "second-equal", "late"}) {
		t.Fatalf("hook order = %v", order)
	}
}

func TestHookRegistryNilResultPreservesValueAndErrorStopsChain(t *testing.T) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session", "agent")
	sentinel := errors.New("hook rejected")
	if err := registry.Register(HookPreLoop, -1, func(context.Context, any) (any, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(HookPreLoop, 0, func(context.Context, any) (any, error) {
		return nil, sentinel
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := registry.Register(HookPreLoop, 1, func(context.Context, any) (any, error) {
		called = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	value, err := registry.trigger(context.Background(), HookPreLoop, "kept")
	if value != "kept" || !errors.Is(err, sentinel) || called {
		t.Fatalf("trigger() = %v, %v, called %v", value, err, called)
	}
}

func TestHookRegistryIsolatesPanicAndPublishesFailure(t *testing.T) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session-1", "agent-1")
	failures := make(chan HookFailure, 1)
	unsubscribe := bus.Subscribe(EventTypeError, func(_ context.Context, event Event) {
		var failure HookFailure
		if err := json.Unmarshal(event.DataBytes(), &failure); err != nil {
			t.Errorf("Unmarshal() error = %v", err)
		}
		failures <- failure
	})
	defer unsubscribe()
	if err := registry.Register(HookPreModelCall, 7, func(context.Context, any) (any, error) {
		panic("broken hook")
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(HookPreModelCall, 8, func(_ context.Context, data any) (any, error) {
		return data.(string) + "-continued", nil
	}); err != nil {
		t.Fatal(err)
	}
	value, err := registry.trigger(nil, HookPreModelCall, "value")
	if err != nil || value != "value-continued" {
		t.Fatalf("trigger() = %v, %v", value, err)
	}
	select {
	case event := <-failures:
		if event.Type != HookPreModelCall || event.Priority != 7 || event.PanicValue != "broken hook" {
			t.Fatalf("hook failure = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for hook failure event")
	}
	registry.publishFailure(HookFailure{Type: HookOnEvent})
	originalMarshal := marshalEventData
	marshalEventData = func(any) ([]byte, error) { return nil, errors.New("marshal failure") }
	registry.publishFailure(HookFailure{Type: HookPreLoop})
	marshalEventData = originalMarshal
}

func TestHookRegistryBoundsFailureBuffer(t *testing.T) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session", "agent")
	if err := registry.Register(HookPreLoop, 0, func(context.Context, any) (any, error) {
		panic("failure")
	}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maximumHookFailures+1; index++ {
		if _, err := registry.trigger(context.Background(), HookPreLoop, index); err != nil {
			t.Fatal(err)
		}
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	if len(registry.failures) != maximumHookFailures {
		t.Fatalf("failure count = %d, want %d", len(registry.failures), maximumHookFailures)
	}
	if registry.failures[0].PanicValue != "failure" || registry.droppedFailures != 1 {
		t.Fatalf("bounded failures = %#v, dropped = %d", registry.failures[0], registry.droppedFailures)
	}
}

func TestHookOnEventIsAsynchronousAndFailureIsolated(t *testing.T) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session", "agent")
	started := make(chan struct{})
	release := make(chan struct{})
	observed := make(chan Event, 2)
	var blockingOnce sync.Once
	blockingHook := HookFunc(func(_ context.Context, data any) (any, error) {
		blockingOnce.Do(func() { close(started) })
		<-release
		return data, nil
	})
	if err := registry.Register(HookOnEvent, 0, HookFunc(blockingHook)); err != nil {
		t.Fatal(err)
	}
	failingHook := HookFunc(func(_ context.Context, data any) (any, error) {
		event := data.(Event)
		observed <- event
		if event.Type == EventTypeDone {
			return nil, errors.New("observer failed")
		}
		panic("observer panicked")
	})
	if err := registry.Register(HookOnEvent, 1, HookFunc(failingHook)); err != nil {
		t.Fatal(err)
	}

	bus.Publish(context.Background(), Event{Type: EventTypeError})
	waitForSignal(t, started)
	close(release)
	bus.Publish(context.Background(), Event{Type: EventTypeDone})
	waitForEvent(t, observed)
	waitForEvent(t, observed)
	deadline := time.Now().Add(time.Second)
	for {
		registry.mu.RLock()
		failureCount := len(registry.failures)
		failures := append([]HookFailure(nil), registry.failures...)
		registry.mu.RUnlock()
		if failureCount == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("on_event failures = %#v, want 2", failures)
		}
		time.Sleep(time.Millisecond)
	}
	if err := registry.Unregister(HookOnEvent, HookFunc(blockingHook)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Unregister(HookOnEvent, HookFunc(failingHook)); err != nil {
		t.Fatal(err)
	}
}

func TestHookRegistryUnregisterValidationAndMissingHook(t *testing.T) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session", "agent")
	hook := func(_ context.Context, value any) (any, error) { return value, nil }
	if err := registry.Unregister("invalid", hook); !errors.Is(err, ErrInvalidHookType) {
		t.Fatalf("Unregister(invalid) error = %v", err)
	}
	if err := registry.Unregister(HookPreLoop, nil); !errors.Is(err, ErrNilHook) {
		t.Fatalf("Unregister(nil) error = %v", err)
	}
	if err := registry.Unregister(HookPreLoop, hook); !errors.Is(err, ErrHookNotRegistered) {
		t.Fatalf("Unregister(missing) error = %v", err)
	}
	if err := registry.Register(HookPreLoop, 0, hook); err != nil {
		t.Fatal(err)
	}
	if err := registry.Unregister(HookPreLoop, hook); err != nil {
		t.Fatal(err)
	}
	value, err := registry.trigger(context.Background(), HookPreLoop, "value")
	if err != nil || value != "value" {
		t.Fatalf("trigger() after unregister = %v, %v", value, err)
	}
}

func TestHookRegistryConcurrentLifecycle(t *testing.T) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session", "agent")
	hook := func(_ context.Context, value any) (any, error) { return value, nil }
	var waitGroup sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		waitGroup.Add(1)
		go func(worker int) {
			defer waitGroup.Done()
			for iteration := 0; iteration < 100; iteration++ {
				hookType := HookType(HookPreLoop)
				if worker%2 == 1 {
					hookType = HookPostLoop
				}
				_ = registry.Register(hookType, iteration%201-100, hook)
				_, _ = registry.trigger(context.Background(), hookType, iteration)
				_ = registry.Unregister(hookType, hook)
			}
		}(worker)
	}
	waitGroup.Wait()
}

func TestValidHookTypeCoversFrozenBaseline(t *testing.T) {
	types := []HookType{
		HookPreLoop,
		HookPostLoop,
		HookPreModelCall,
		HookPostModelCall,
		HookToolCallReceived,
		HookPreToolExec,
		HookPostToolExec,
		HookToolResult,
		HookPreCompact,
		HookPostCompact,
		HookContextInject,
		HookSubAgentSpawn,
		HookSubAgentDone,
		HookOnEvent,
	}
	for _, hookType := range types {
		if !validHookType(hookType) {
			t.Errorf("validHookType(%q) = false", hookType)
		}
	}
	if validHookType(HookType(fmt.Sprintf("unknown-%d", time.Now().UnixNano()))) {
		t.Fatal("unknown hook type is valid")
	}
}
