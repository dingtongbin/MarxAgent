// SPDX-License-Identifier: Apache-2.0

package lsp

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// A language server is started lazily and then held for the rest of a session, so
// what has to be fast is the framing and the dispatch rather than the launch. A
// tool call that blocks on framing would look to a user like the server hung.

func TestLSPPerformanceGates(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to run the language server performance gates")
	}
	t.Run("framing_a_message_is_linear", func(t *testing.T) {
		// The framing is on the path of every message in both directions, and the
		// obvious implementation copies the payload more than once.
		pool := newTooledPool(t)
		_ = pool
		small := Message{JSONRPC: "2.0", ID: ID{Number: 1}, Method: MethodTextDocumentHover}
		large := Message{
			JSONRPC: "2.0", ID: ID{Number: 1}, Method: MethodTextDocumentHover,
			Params: []byte(`{"textDocument":{"uri":"file:///a.go"},"position":{"line":1,"character":1},
				"padding":"` + strings.Repeat("x", 60000) + `"}`),
		}
		smallFrame := frameMessage(small)
		largeFrame := frameMessage(large)
		// The overhead is the header, and it does not grow with the body.
		smallOverhead := len(smallFrame) - len(small.Params)
		largeOverhead := len(largeFrame) - len(large.Params)
		if smallOverhead <= 0 {
			t.Fatalf("the header cost %d bytes", smallOverhead)
		}
		if largeOverhead > smallOverhead*2 {
			t.Fatalf("the header grew with the body: %d then %d", smallOverhead, largeOverhead)
		}
	})
	t.Run("a_request_survives_a_large_document", func(t *testing.T) {
		// Opening a file tells the server its whole contents, so the cost of a
		// request has to stay independent of how big the file is.
		pool := newTooledPool(t)
		contents := strings.Repeat("package main\n", 20000)
		started := time.Now()
		if err := pool.Open("/project/big.go", contents); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("opening a large document took %v", elapsed)
		}
		// The version advanced, which is what stops the server answering about a
		// version it has already replaced.
		if got := pool.documentVersion(PathToURI("/project/big.go")); got != FirstDocumentVersion {
			t.Fatalf("version = %d", got)
		}
	})
	t.Run("many_open_documents_stay_cheap_to_list", func(t *testing.T) {
		pool := newTooledPool(t)
		for index := 0; index < 200; index++ {
			if err := pool.Open("/project/file"+string(rune('a'+index%26))+".go", "package main\n"); err != nil {
				t.Fatal(err)
			}
		}
		started := time.Now()
		opened := pool.Opened()
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("listing %d documents took %v", len(opened), elapsed)
		}
		if len(opened) == 0 {
			t.Fatal("no documents are open")
		}
	})
	t.Run("a_question_does_not_start_a_server_by_itself", func(t *testing.T) {
		// The laziness is the point: a session that never asks a question should
		// never hold a process.
		starter := workingStarter(t)
		client, err := New(Config{Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		store := client.Diagnostics()
		for index := 0; index < 500; index++ {
			store.Replace("file:///a.go", []Diagnostic{{Severity: SeverityError, Message: "x"}})
		}
		if starter.Launches() != 0 {
			t.Fatalf("launches = %d, reading diagnostics started a server", starter.Launches())
		}
	})
	t.Run("a_request_bounded_by_its_timeout", func(t *testing.T) {
		// A server that never answers must cost the timeout, not more, so a session
		// cannot be held open by a language server that has wedged.
		silent := newFakeServer(t)
		silent.silent = true
		starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
			silent.attach(nopCloser(process.server), process.client)
		}}
		client, err := New(Config{
			Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter,
			RequestTimeout: 50 * time.Millisecond, StartTimeout: 50 * time.Millisecond,
			MaxRestarts: 1, RestartBackoff: time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		started := time.Now()
		if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err == nil {
			t.Fatal("a silent server answered")
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("the request cost %v against a 50ms timeout", elapsed)
		}
	})
}

// frameMessage renders a message the way the writer does, so a test can measure
// the framing without a stream.
func frameMessage(message Message) string {
	var buffer bytes.Buffer
	if err := NewWriter(&buffer).Write(message); err != nil {
		return ""
	}
	return buffer.String()
}

// frameMessageTo is the benchmark form, which cannot fail and so has nothing to
// report.
func frameMessageTo(b *testing.B, message Message) string {
	b.Helper()
	var buffer bytes.Buffer
	if err := NewWriter(&buffer).Write(message); err != nil {
		b.Fatal(err)
	}
	return buffer.String()
}

// nopCloser adapts a reader for the harness, which wants a closeable.
func nopCloser(reader io.Reader) io.ReadCloser { return io.NopCloser(reader) }

func BenchmarkFrameMessage(b *testing.B) {
	message := Message{JSONRPC: "2.0", ID: ID{Number: 1}, Method: MethodTextDocumentHover,
		Params: []byte(`{"textDocument":{"uri":"file:///a.go"},"position":{"line":1,"character":1}}`)}
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		_ = frameMessageTo(b, message)
	}
}

func BenchmarkReadMessage(b *testing.B) {
	message := Message{JSONRPC: "2.0", ID: ID{Number: 1}, Method: MethodTextDocumentHover,
		Params: []byte(`{"textDocument":{"uri":"file:///a.go"},"position":{"line":1,"character":1}}`)}
	reader := NewReader(strings.NewReader(frameMessageTo(b, message)))
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		reader = NewReader(strings.NewReader(frameMessageTo(b, message)))
		if _, err := reader.Read(); err != nil {
			b.Fatal(err)
		}
	}
}
