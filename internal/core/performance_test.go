// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

const (
	maximumEventDispatchP99 = time.Millisecond
	maximumEmptyLoop        = 100 * time.Microsecond
	maximumStateSnapshot    = 100 * time.Microsecond
	maximumHookTrigger      = 50 * time.Microsecond
	maximumAgentMemory      = 10 * 1024 * 1024
)

func TestL1PerformanceGates(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to enforce L1 performance gates")
	}

	t.Run("1000 message snapshot", func(t *testing.T) {
		state := benchmarkState(1000)
		result := testing.Benchmark(func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				_ = state.snapshot()
			}
		})
		if result.NsPerOp() >= maximumStateSnapshot.Nanoseconds() {
			t.Fatalf("1000 message snapshot = %s, limit < %s", time.Duration(result.NsPerOp()), maximumStateSnapshot)
		}
	})

	t.Run("empty loop", func(t *testing.T) {
		result := testing.Benchmark(func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				agent := newBenchmarkAgent(benchmark, emptyStreamProvider())
				events, err := agent.Run(context.Background(), textInput("benchmark"))
				if err != nil {
					benchmark.Fatal(err)
				}
				for range events {
				}
			}
		})
		if result.NsPerOp() >= maximumEmptyLoop.Nanoseconds() {
			t.Fatalf("empty loop = %s, limit < %s", time.Duration(result.NsPerOp()), maximumEmptyLoop)
		}
	})

	t.Run("event dispatch p99 with 100 subscribers", func(t *testing.T) {
		bus := &asyncEventBus{}
		unsubscribers := make([]func(), 100)
		for index := range unsubscribers {
			unsubscribers[index] = bus.Subscribe(EventTypeAll, func(context.Context, Event) {})
		}
		defer func() {
			for _, unsubscribe := range unsubscribers {
				unsubscribe()
			}
			bus.workers.Wait()
		}()
		durations := make([]time.Duration, 1000)
		for index := range durations {
			started := time.Now()
			bus.Publish(context.Background(), Event{Type: EventTypeDone, Data: `{}`})
			durations[index] = time.Since(started)
		}
		sort.Slice(durations, func(left, right int) bool {
			return durations[left] < durations[right]
		})
		p99 := durations[(len(durations)*99)/100]
		if p99 >= maximumEventDispatchP99 {
			t.Fatalf("event dispatch p99 = %s, limit < %s", p99, maximumEventDispatchP99)
		}
	})

	t.Run("10 hook trigger", func(t *testing.T) {
		bus := &asyncEventBus{}
		registry := newHookRegistry(bus, "session", "agent")
		hook := func(_ context.Context, value any) (any, error) { return value, nil }
		for index := 0; index < 10; index++ {
			if err := registry.Register(HookPreLoop, index, hook); err != nil {
				t.Fatal(err)
			}
		}
		result := testing.Benchmark(func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				if _, err := registry.trigger(context.Background(), HookPreLoop, "value"); err != nil {
					benchmark.Fatal(err)
				}
			}
		})
		if result.NsPerOp() >= maximumHookTrigger.Nanoseconds() {
			t.Fatalf("10 hook trigger = %s, limit < %s", time.Duration(result.NsPerOp()), maximumHookTrigger)
		}
	})

	t.Run("10k message agent memory", func(t *testing.T) {
		result := testing.Benchmark(func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				_ = benchmarkState(10000)
			}
		})
		if result.AllocedBytesPerOp() >= maximumAgentMemory {
			t.Fatalf("10k message agent allocation = %d bytes, limit < %d", result.AllocedBytesPerOp(), maximumAgentMemory)
		}
	})
}

func BenchmarkEventBusPublish100Subscribers(benchmark *testing.B) {
	bus := &asyncEventBus{}
	unsubscribers := make([]func(), 100)
	for index := range unsubscribers {
		unsubscribers[index] = bus.Subscribe(EventTypeAll, func(context.Context, Event) {})
	}
	defer func() {
		for _, unsubscribe := range unsubscribers {
			unsubscribe()
		}
	}()
	event := Event{Type: EventTypeDone, Data: `{}`}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for iteration := 0; iteration < benchmark.N; iteration++ {
		bus.Publish(context.Background(), event)
	}
}

func BenchmarkAgentEmptyLoop(benchmark *testing.B) {
	benchmark.ReportAllocs()
	for iteration := 0; iteration < benchmark.N; iteration++ {
		agent := newBenchmarkAgent(benchmark, emptyStreamProvider())
		events, err := agent.Run(context.Background(), textInput("benchmark"))
		if err != nil {
			benchmark.Fatal(err)
		}
		for range events {
		}
	}
}

func BenchmarkStateSnapshot1000Messages(benchmark *testing.B) {
	state := benchmarkState(1000)
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for iteration := 0; iteration < benchmark.N; iteration++ {
		_ = state.snapshot()
	}
}

func BenchmarkHookTrigger10Hooks(benchmark *testing.B) {
	bus := &asyncEventBus{}
	registry := newHookRegistry(bus, "session", "agent")
	hook := func(_ context.Context, value any) (any, error) { return value, nil }
	for index := 0; index < 10; index++ {
		if err := registry.Register(HookPreLoop, index, hook); err != nil {
			benchmark.Fatal(err)
		}
	}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for iteration := 0; iteration < benchmark.N; iteration++ {
		_, _ = registry.trigger(context.Background(), HookPreLoop, "value")
	}
}

func BenchmarkAgentMemory10000Messages(benchmark *testing.B) {
	benchmark.ReportAllocs()
	for iteration := 0; iteration < benchmark.N; iteration++ {
		_ = benchmarkState(10000)
	}
}

func newBenchmarkAgent(benchmark *testing.B, provider Provider) Agent {
	benchmark.Helper()
	agent, err := NewAgent(provider, Config{Model: "benchmark"})
	if err != nil {
		benchmark.Fatal(err)
	}
	return agent
}

func benchmarkState(messageCount int) *state {
	messages := make([]Message, messageCount)
	for index := range messages {
		messages[index] = validTestMessage("benchmark-" + strconv.Itoa(index))
	}
	return newState(Config{AgentID: "benchmark", InitialMessages: messages})
}
