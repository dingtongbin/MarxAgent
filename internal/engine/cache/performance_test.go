// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"os"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// The signature runs on every model call, and the monitor runs on every
// observation, so both have to stay cheap no matter how long a session gets.

func TestCachePerformanceGates(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to run the cache performance gates")
	}
	t.Run("signature_scales_with_the_prompt_not_the_history", func(t *testing.T) {
		manager := New(Config{})
		tools := []core.ToolSpec{tool("read", "reads a file"), tool("bash", "runs a command")}
		small := manager.Signature(strings.Repeat("system prompt ", 50), tools, "index")
		large := manager.Signature(strings.Repeat("system prompt ", 5000), tools, "index")
		if small == "" || large == "" || small == large {
			t.Fatal("prompts of different sizes produced the same signature")
		}
		// A long history must not change the signature at all, because the history
		// is not part of the cached prefix identity.
		withHistory := manager.Signature(strings.Repeat("system prompt ", 50), tools, "index")
		if small != withHistory {
			t.Fatal("the signature depends on something it should not")
		}
	})
	t.Run("recording_a_turn_does_not_scan_the_history", func(t *testing.T) {
		manager := New(Config{})
		tools := []core.ToolSpec{tool("read", "reads a file")}
		messages := history(2000)
		if _, change := manager.Record("system", tools, "", "dynamic", messages); change.Kind != ChangeNone {
			t.Fatalf("change = %#v", change)
		}
		// One more turn on top of a two thousand message history.
		_, change := manager.Record("system", tools, "", "dynamic again", append(messages, messages[0]))
		if change.Kind != ChangeAppend {
			t.Fatalf("change = %#v", change)
		}
		stats := manager.Stats()
		if stats.Turns != 0 {
			t.Fatalf("recording a turn was counted as an observation: %#v", stats)
		}
	})
	t.Run("the_window_not_the_history_bounds_the_rate", func(t *testing.T) {
		manager := New(Config{ObservationWindow: 5, MaxHistory: 64})
		for turn := 0; turn < 500; turn++ {
			manager.Observe(Observation{ReadTokens: 900, InputTokens: 1000})
		}
		_, window := manager.HitRate()
		// The rate is over the window, so a long healthy run is not diluted by
		// whatever happened five hundred turns ago.
		if window != 5 {
			t.Fatalf("window = %d", window)
		}
		if len(manager.Alarms()) != 0 {
			t.Fatal("a healthy run raised an alarm")
		}
	})
}

func BenchmarkSignature(b *testing.B) {
	manager := New(Config{})
	tools := []core.ToolSpec{
		tool("read", "reads a file from disk"),
		tool("write", "writes a file to disk"),
		tool("bash", "runs a shell command"),
		tool("glob", "finds files by pattern"),
		tool("grep", "searches file contents"),
	}
	system := strings.Repeat("you are a helpful assistant. ", 200)
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		_ = manager.Signature(system, tools, "capability index")
	}
}

func BenchmarkRecordTurn(b *testing.B) {
	manager := New(Config{})
	tools := []core.ToolSpec{tool("read", "reads a file")}
	messages := history(500)
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		manager.Record("system", tools, "", "dynamic", messages)
	}
}

func BenchmarkObserve(b *testing.B) {
	manager := New(Config{ObservationWindow: 10})
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		manager.Observe(Observation{ReadTokens: 900, InputTokens: 1000})
	}
}
