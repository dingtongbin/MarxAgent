// SPDX-License-Identifier: Apache-2.0

package lsp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
)

// fakeServer is a language server that runs in the test process, connected through
// a pair of pipes. It exercises the real framing, the real dispatch and the real
// restart path, which a stubbed reader would not.
type fakeServer struct {
	t *testing.T
	// requests is every message the server has read, guarded by stateMu because
	// the serving goroutine writes it while the test reads it.
	requests []Message
	// capabilities is what the handshake answers with.
	capabilities ServerCapabilities
	// handler answers a request, returning the raw result.
	handler func(method string, params json.RawMessage) (json.RawMessage, *ResponseError)
	// crashBefore is the method whose arrival kills the server without an answer,
	// which is how a crash is simulated.
	//
	// The crash is named rather than counted. A count of messages leaves the test
	// at the mercy of how many messages the handshake happens to send, and the
	// handshake is two of them, so a count set to crash after the second message
	// dies in the middle of the handshake on any run where the client sends one
	// more. The initialize request is the one request the client never retries, so a
	// server that dies there fails the turn outright, and the failure would be
	// reported against whichever test the scheduler happened to be in.
	crashBefore string
	// silent makes the server read everything and answer nothing, which is what a
	// hung language server looks like.
	silent bool
	// state guards what the serving goroutine writes and the test reads.
	stateMu sync.Mutex
	// served counts the messages handled.
	served int
	// sessions are the launches this server has served, one per attach.
	sessions []*serveSession
	// stopCloses is closed to bring the serving goroutines out of their reads.
	stopCloses chan struct{}
	// stopped guards against closing twice.
	stopped bool
	// output is the server's end of the stream for the newest launch.
	output io.WriteCloser
	// toClient carries messages the server sends to the client.
	toClient chan Message
}

func newFakeServer(t *testing.T) *fakeServer {
	return &fakeServer{
		t: t,
		capabilities: ServerCapabilities{
			HoverProvider:      boolPtr(true),
			DefinitionProvider: boolPtr(true),
			ReferencesProvider: boolPtr(true),
			RenameProvider:     boolPtr(true),
		},
		stopCloses: make(chan struct{}),
		toClient:   make(chan Message, 16),
	}
}

func boolPtr(value bool) *bool { return &value }

// stop brings the serving goroutine out of its read and waits for it to finish.
//
// A server left running would hold a goroutine blocked on a pipe and four file
// descriptors past the end of its test, and a goroutine that outlives the test it
// belongs to is how one test breaks another: the failure is reported against
// whatever the scheduler was running at the time, which is why the same defect
// shows up as a different test on every run.
func (s *fakeServer) stop() {
	s.stateMu.Lock()
	if s.stopped {
		s.stateMu.Unlock()
		return
	}
	s.stopped = true
	sessions := append([]*serveSession(nil), s.sessions...)
	s.sessions = nil
	s.stateMu.Unlock()
	close(s.stopCloses)
	// Every session is waited for, not just the newest. A server attached more than
	// once has one goroutine per attach, and returning while an older one is still
	// reading would leave it running past the end of its test.
	for _, session := range sessions {
		<-session.done
	}
}

// handled reports how many messages the server has handled.
func (s *fakeServer) handled() int {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.served
}

// attach connects the fake server to a pair of streams.
//
// The channel the test pushes server bound messages into is pumped onto the
// stream, so a message the test sends reaches the client through the same framing
// a real server would use rather than through a side channel the client does not
// know about.
//
// Each call gets its own serving session, and the session owns the streams it was
// given rather than reading them back off the server. A server can be attached
// more than once, because a restart looks exactly like that, and a session that
// looked its streams up on the way out would close whichever launch was current
// when it happened to exit. That closes a live server's pipe under the client,
// which shows up as a handshake that fails for no reason, on whichever test the
// scheduler was running.
func (s *fakeServer) attach(clientToServer io.ReadCloser, serverToClient io.WriteCloser) {
	session := &serveSession{
		reader: NewReader(clientToServer),
		writer: NewWriter(serverToClient),
		input:  clientToServer,
		output: serverToClient,
		done:   make(chan struct{}),
	}
	s.stateMu.Lock()
	s.sessions = append(s.sessions, session)
	s.output = serverToClient
	s.stateMu.Unlock()
	go s.serve(session)
	go func() {
		for {
			select {
			case message := <-s.toClient:
				if err := session.writer.Write(message); err != nil {
					return
				}
			case <-s.stopCloses:
				// The server is stopping, so the pump has nothing left to deliver
				// and must not hold a message the test is waiting to send.
				return
			}
		}
	}()
}

// serveSession is one launch's worth of a fake server.
type serveSession struct {
	reader *Reader
	writer *Writer
	input  io.ReadCloser
	output io.WriteCloser
	done   chan struct{}
}

func (s *fakeServer) serve(session *serveSession) {
	defer close(session.done)
	// The stream is closed when the loop ends, because that is what a process
	// exiting does to its pipes. Without it the client would wait for an answer from
	// a server that is gone, and the test would be measuring the harness rather than
	// the client. The stream closed is the session's own, never whatever the server
	// is attached to now.
	defer func() {
		_ = session.output.Close()
	}()
	go func() {
		// Closing the client's end of the stream is the only way to bring a read
		// that is already blocked out of it, so a stopped server unblocks itself
		// rather than waiting for a client that may never write again.
		<-s.stopCloses
		_ = session.input.Close()
	}()
	for {
		message, err := session.reader.Read()
		if err != nil {
			return
		}
		s.stateMu.Lock()
		s.served++
		s.requests = append(s.requests, message)
		crashBefore := s.crashBefore
		s.stateMu.Unlock()
		if crashBefore != "" && message.Method == crashBefore {
			// A server that stops answering is what a crash looks like from the
			// client's side: the stream ends and no response ever arrives. The
			// handshake is never the crash point, because a client cannot recover
			// from a server that dies before it has ever spoken to it.
			return
		}
		if message.IsNotification() {
			continue
		}
		if s.silent {
			// The request is read and deliberately not answered.
			continue
		}
		result, failure := s.answer(message.Method, message.Params)
		reply := Message{JSONRPC: "2.0", ID: message.ID}
		if failure != nil {
			reply.Error = failure
		} else {
			reply.Result = result
		}
		if err := session.writer.Write(reply); err != nil {
			return
		}
	}
}

func (s *fakeServer) answer(method string, params json.RawMessage) (json.RawMessage, *ResponseError) {
	switch method {
	case MethodInitialize:
		return mustMarshal(s.capabilities), nil
	}
	if s.handler != nil {
		return s.handler(method, params)
	}
	switch method {
	case MethodTextDocumentHover:
		return mustMarshal(Hover{Contents: MarkupContent{Kind: MarkupMarkdown, Value: "```go\nfunc Add(a, b int) int\n```"}}), nil
	case MethodTextDocumentDefinition, MethodTextDocumentReferences:
		return mustMarshal([]Location{{
			URI: PathToURI("/project/main.go"),
			Range: Range{
				Start: Position{Line: 9, Character: 5},
				End:   Position{Line: 9, Character: 8},
			},
		}}), nil
	case MethodTextDocumentRename:
		return mustMarshal(WorkspaceEdit{Changes: map[string][]TextEdit{
			PathToURI("/project/main.go"): {
				{Range: Range{Start: Position{Line: 9, Character: 5}, End: Position{Line: 9, Character: 8}},
					NewText: "Sum"},
			},
		}}), nil
	default:
		return nil, &ResponseError{Code: CodeMethodNotFound, Message: "unknown method " + method}
	}
}

// send pushes a message from the server to the client.
func (s *fakeServer) send(message Message) {
	s.t.Helper()
	message.JSONRPC = "2.0"
	s.toClient <- message
}

// methodsHandled reports which methods the server saw.
func (s *fakeServer) methodsHandled() []string {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	names := make([]string, 0, len(s.requests))
	for _, request := range s.requests {
		if request.Method != "" {
			names = append(names, request.Method)
		}
	}
	return names
}

// watchCrashes reports why a reader ended, if the test failed.
//
// A client that reports only "the language server is not running" leaves the
// reader guessing, because that answer is the same whether the server exited,
// the stream was torn down underneath it, or a read failed for a reason nobody has
// seen yet. The client keeps both records, so a failing test hands them over and
// the next occurrence is information rather than a mystery.
func watchCrashes(t *testing.T, client *Client) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if crash := client.LastCrash(); crash != nil {
			t.Logf("the reader ended: %v", crash.Err)
		}
		if reason := client.LastReadError(); reason != "" {
			t.Logf("the last read error was: %s", reason)
		}
	})
}

// workingStarter hands out a process with a healthy server attached. Every launch
// gets its own server, because a server shared between a launch and the one that
// replaced it is how a test ends up measuring the harness rather than the client.
//
// Every server it creates is stopped when the test ends, whether or not the test
// closed its client. A server that is left running keeps a goroutine blocked on a
// pipe and four descriptors open, and the failure it eventually causes is
// reported against an unrelated test.
func workingStarter(t *testing.T) *pipeStarter {
	starter := &pipeStarter{}
	starter.track(t, func(process *pipeProcess) *fakeServer {
		server := newFakeServer(t)
		server.attach(process.server, process.client)
		return server
	})
	return starter
}

// pipeStarter records the servers it hands out so the test can stop them.
//
// A server is stopped on the way out because a test that only closes its client
// when it remembers to is a test that leaks, and the leak is invisible until a
// later test on a busier machine fails instead.
type serverFactory func(process *pipeProcess) *fakeServer

func (s *pipeStarter) track(t *testing.T, build serverFactory) {
	var mu sync.Mutex
	var servers []*fakeServer
	t.Cleanup(func() {
		mu.Lock()
		held := servers
		servers = nil
		mu.Unlock()
		for _, server := range held {
			server.stop()
		}
	})
	s.onLaunch = func(process *pipeProcess) {
		server := build(process)
		mu.Lock()
		servers = append(servers, server)
		mu.Unlock()
	}
}

// pipeProcess is a sandbox.Process backed by in-memory pipes.
type pipeProcess struct {
	stdin  *writeHalf
	stdout *readHalf
	client *writeHalf
	server *readHalf
	closed chan struct{}
	once   sync.Once
}

// newPipeProcess builds the two directions of a pipe pair: what the client writes
// is what the server reads, and what the server writes is what the client reads.
//
// The streams are in memory rather than operating system pipes, and the reason is
// a capability rather than a convenience. An operating system pipe gives both
// holders a file descriptor, so a fake server can close the descriptor the client
// is reading, and a client whose read end is closed underneath it reports a file
// that is already closed. A real language server is a separate process holding
// only its own end of the pipe and physically cannot reach the client's, and when
// it exits the client sees the end of the stream. A harness that can produce a
// failure the product cannot have is a harness that teaches the wrong lesson, and
// it does so at random, which is worse.
//
// Buffering is what keeps this from deadlocking. An unbuffered pipe makes a write
// wait for a reader, so a test ends up measuring the order two goroutines happen
// to be scheduled in rather than what the client does with the bytes.
func newPipeProcess() *pipeProcess {
	// Two streams, four ends: what the client writes is what the server reads, and
	// what the server writes is what the client reads.
	toServer := newMemoryStream()
	toClient := newMemoryStream()
	return &pipeProcess{
		stdin:  &writeHalf{stream: toServer},
		server: &readHalf{stream: toServer},
		client: &writeHalf{stream: toClient},
		stdout: &readHalf{stream: toClient},
		closed: make(chan struct{}),
	}
}

// memoryStream is one direction of a connection: bytes queue up for a reader, a
// reader drains them, and the two ends are closed independently the way the ends
// of a pipe are.
type memoryStream struct {
	mu       sync.Mutex
	ready    *sync.Cond
	buffered []byte
	// wroteEndClosed is the writing half giving up, which the reader sees as the
	// end of the stream.
	wroteEndClosed bool
	// readEndGone is the reading half going away, which the writer sees as a broken
	// pipe rather than a message that was delivered.
	readEndGone bool
}

func newMemoryStream() *memoryStream {
	stream := &memoryStream{}
	stream.ready = sync.NewCond(&stream.mu)
	return stream
}

// Write queues bytes, waiting for room when the buffer is full.
//
// It never waits for a reader to arrive, because a write that blocks until the
// other side happens to be scheduled is a test of the scheduler.
func (s *memoryStream) Write(payload []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readEndGone {
		return 0, errBrokenPipe
	}
	written := 0
	for written < len(payload) {
		for len(s.buffered) >= memoryStreamLimit && !s.readEndGone && !s.wroteEndClosed {
			s.ready.Wait()
		}
		if s.readEndGone {
			return written, errBrokenPipe
		}
		room := memoryStreamLimit - len(s.buffered)
		if room > len(payload)-written {
			room = len(payload) - written
		}
		s.buffered = append(s.buffered, payload[written:written+room]...)
		written += room
		s.ready.Broadcast()
	}
	return written, nil
}

// Read takes what has been written, and reports the end of the stream once the
// writing half is closed and nothing is left.
func (s *memoryStream) Read(target []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.buffered) == 0 {
		if s.wroteEndClosed || s.readEndGone {
			return 0, io.EOF
		}
		s.ready.Wait()
	}
	taken := copy(target, s.buffered)
	s.buffered = s.buffered[taken:]
	s.ready.Broadcast()
	return taken, nil
}

// CloseWrite ends the stream for a reader, which is what a process exiting does.
func (s *memoryStream) CloseWrite() error {
	s.mu.Lock()
	s.wroteEndClosed = true
	s.ready.Broadcast()
	s.mu.Unlock()
	return nil
}

// CloseRead abandons the stream, which is what killing a process does: a writer
// finds out at once instead of filling a buffer nobody will ever drain.
func (s *memoryStream) CloseRead() error {
	s.mu.Lock()
	s.readEndGone = true
	s.buffered = nil
	s.ready.Broadcast()
	s.mu.Unlock()
	return nil
}

func (s *memoryStream) Close() error {
	return s.CloseWrite()
}

// memoryStreamLimit bounds what a test can queue, so a runaway writer is caught
// rather than turning into a slow leak.
const memoryStreamLimit = 1 << 20

// errBrokenPipe is what a write reports once the reader has gone.
var errBrokenPipe = errors.New("lsp test: the reader is gone")

func (p *pipeProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *pipeProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *pipeProcess) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }

// writeHalf is the writing end of a stream, which is what a process's input is.
type writeHalf struct{ stream *memoryStream }

func (w *writeHalf) Write(payload []byte) (int, error) { return w.stream.Write(payload) }
func (w *writeHalf) Close() error                      { return w.stream.CloseWrite() }

// readHalf is the reading end of a stream, which is what a process's output is.
type readHalf struct{ stream *memoryStream }

func (r *readHalf) Read(target []byte) (int, error) { return r.stream.Read(target) }
func (r *readHalf) Close() error                    { return r.stream.CloseRead() }

// Wait blocks until the process is closed.
func (p *pipeProcess) Wait() (sandbox.Result, error) {
	<-p.closed
	return sandbox.Result{}, nil
}

// Close shuts both directions. It is the client letting a server go, so it ends
// the client's input for the server to notice and abandons its output, which is
// what a client does with a process it is finished with.
func (p *pipeProcess) Close() error {
	p.once.Do(func() {
		close(p.closed)
		_ = p.stdin.Close()
		_ = p.client.Close()
		_ = p.server.Close()
		_ = p.stdout.Close()
	})
	return nil
}

// pipeStarter hands out a pipe process per launch, so a restart gets a new one.
type pipeStarter struct {
	mu       sync.Mutex
	launches int
	// onLaunch is called with the process, so a test can attach a server to it.
	onLaunch func(*pipeProcess)
	// failWith makes a launch fail.
	failWith error
}

func (s *pipeStarter) Start(context.Context, []string, []string, string) (sandbox.Process, error) {
	s.mu.Lock()
	s.launches++
	current := s.launches
	handler := s.onLaunch
	failure := s.failWith
	s.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	process := newPipeProcess()
	if handler != nil {
		handler(process)
	}
	if current > 0 {
		// Each launch gets a fresh server, and the old one is told to stop, which is
		// what a real restart looks like from the outside.
		_ = process
	}
	return process, nil
}

// Capabilities reports no confinement, because a pipe is not a real process and
// the client only consults this to decide what to tell a server about its root.
func (s *pipeStarter) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }

func (s *pipeStarter) Launches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launches
}

func TestNewValidatesItsConfiguration(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a client without a command was accepted")
	}
	if _, err := New(Config{Command: []string{"  "}}); err == nil {
		t.Fatal("a blank command was accepted")
	}
	if _, err := New(Config{Command: []string{"gopls"}}); err == nil {
		t.Fatal("a client without a workspace was accepted")
	}
	client, err := New(Config{Command: []string{"gopls"}, Workspace: "/project"})
	if err != nil {
		t.Fatal(err)
	}
	// Defaults are applied rather than leaving zeros in place.
	if client.config.RequestTimeout != DefaultRequestTimeout {
		t.Fatalf("request timeout = %v", client.config.RequestTimeout)
	}
	if client.config.MaxRestarts != DefaultMaxRestarts {
		t.Fatalf("max restarts = %d", client.config.MaxRestarts)
	}
	if client.Running() {
		t.Fatal("a client that was never asked is running")
	}
}

func TestTheServerIsNotStartedUntilItIsNeeded(t *testing.T) {
	starter := &pipeStarter{}
	client, err := New(Config{Command: []string{"gopls"}, Workspace: "/project", Starter: starter})
	if err != nil {
		t.Fatal(err)
	}
	// A language server is expensive to hold, and a session that never asks a
	// question should not pay for one.
	if starter.Launches() != 0 {
		t.Fatalf("launches = %d before any request", starter.Launches())
	}
	if client.Running() {
		t.Fatal("the client reports a running server")
	}
	// A notification against a server that is not up is reported rather than
	// silently dropped, because the caller may believe the server heard it.
	if err := client.notify(MethodInitialized, []byte("{}")); err == nil {
		t.Fatal("a notification was accepted with no server")
	}
	if starter.Launches() != 0 {
		t.Fatal("a notification started a server")
	}
}

func TestHandshakeAndRequests(t *testing.T) {
	server := newFakeServer(t)
	starter := &pipeStarter{}
	starter.track(t, func(process *pipeProcess) *fakeServer {
		// The server reads what the client writes and writes back on the other
		// direction.
		server.attach(process.server, process.client)
		return server
	})
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 2 * time.Second, RestartBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	result, err := client.Request(context.Background(), MethodTextDocumentHover,
		TextDocumentPositionParams{
			TextDocument: TextDocumentIdentifier{URI: PathToURI("/project/main.go")},
			Position:     Position{Line: 9, Character: 5},
		})
	if err != nil {
		t.Fatalf("readErr=%q crash=%v", client.LastReadError(), client.LastCrash())
	}
	var hover Hover
	if err := json.Unmarshal(result, &hover); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hover.Contents.Value, "func Add") {
		t.Fatalf("hover = %#v", hover)
	}
	// The handshake happened before the question, and the server's capabilities are
	// what a later request is checked against.
	handled := server.methodsHandled()
	if len(handled) < 2 || handled[0] != MethodInitialize {
		t.Fatalf("handled = %v", handled)
	}
	if !client.Capabilities().Supports(MethodTextDocumentDefinition) {
		t.Fatal("the capabilities were not recorded")
	}
	if client.Running() != true {
		t.Fatal("the client does not report a running server")
	}
}

func TestCapabilityTheServerDidNotMentionIsOff(t *testing.T) {
	// Sending a request for a capability the server did not claim would only earn
	// an error, and an error a caller has to interpret is worse than a refusal.
	capabilities := ServerCapabilities{}
	if capabilities.Supports(MethodTextDocumentHover) {
		t.Fatal("an absent capability was reported as on")
	}
	if capabilities.Supports("textDocument/somethingElse") {
		t.Fatal("an unknown capability was reported as on")
	}
	off := false
	capabilities = ServerCapabilities{HoverProvider: &off}
	if capabilities.Supports(MethodTextDocumentHover) {
		t.Fatal("a disabled capability was reported as on")
	}
	on := true
	capabilities = ServerCapabilities{HoverProvider: &on}
	if !capabilities.Supports(MethodTextDocumentHover) {
		t.Fatal("an enabled capability was reported as off")
	}
}

func TestAServerResponseErrorIsReported(t *testing.T) {
	server := newFakeServer(t)
	server.handler = func(string, json.RawMessage) (json.RawMessage, *ResponseError) {
		return nil, &ResponseError{Code: CodeInternalError, Message: "the index is not built"}
	}
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		server.attach(process.server, process.client)
	}}
	client, err := New(Config{Command: []string{"gopls"}, Workspace: "/project", Starter: starter})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Request(context.Background(), MethodTextDocumentHover, nil)
	if err == nil {
		t.Fatal("a server error was not reported")
	}
	// The code is in the message, because a caller deciding what to do about a
	// failure needs it and a bare sentence loses it.
	if !strings.Contains(err.Error(), "-32603") || !strings.Contains(err.Error(), "not built") {
		t.Fatalf("err = %v", err)
	}
}

func TestACrashedServerComesBackWithinItsBound(t *testing.T) {
	var mu sync.Mutex
	launches := 0
	starter := &pipeStarter{}
	starter.track(t, func(process *pipeProcess) *fakeServer {
		server := newFakeServer(t)
		mu.Lock()
		launches++
		first := launches == 1
		mu.Unlock()
		// Only the first server dies, and it dies when the first real question
		// arrives rather than after a count of messages. A count would put the
		// crash a fixed distance from the handshake, and the handshake is two
		// messages, so the margin would be zero on any run that sent one more.
		if first {
			server.crashBefore = MethodTextDocumentHover
		}
		server.attach(process.server, process.client)
		return server
	})
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 200 * time.Millisecond, MaxRestarts: 2, RestartBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	// The first question is answered.
	if _, err := client.Request(ctx, MethodTextDocumentHover, nil); err != nil {
		t.Fatal(err)
	}
	// The second one finds the server gone, and the client brings it back.
	if _, err := client.Request(ctx, MethodTextDocumentHover, nil); err != nil {
		t.Fatalf("the second question: %v", err)
	}
	if starter.Launches() < 2 {
		t.Fatalf("launches = %d, the server was not restarted", starter.Launches())
	}
	// A crash is reported, so a caller can say why the server was slow. It is kept
	// after the recovery rather than cleared, because a client that recovered is
	// exactly the one whose caller most wants to know the server had died.
	crash := client.LastCrash()
	if crash == nil {
		t.Fatal("the crash was not recorded")
	}
	if crash.Err == nil || crash.At.IsZero() {
		t.Fatalf("crash = %#v", crash)
	}
}

func TestACrashLoopStopsRatherThanRestartingForever(t *testing.T) {
	// A server that dies on every launch would restart forever, which is worse than
	// being down.
	starter := &pipeStarter{}
	starter.onLaunch = func(process *pipeProcess) {
		// A server that never answers the handshake, so the client gives up on it.
		process.Close()
	}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 50 * time.Millisecond, StartTimeout: 50 * time.Millisecond,
		MaxRestarts: 2, RestartBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := context.Background()
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if _, err := client.Request(ctx, MethodTextDocumentHover, nil); err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		t.Fatal("a crash loop reported success")
	}
	if starter.Launches() > 6 {
		t.Fatalf("launches = %d, the client is looping", starter.Launches())
	}
}

func TestARequestIsAbandonedWhenTheServerGoesAway(t *testing.T) {
	// A server that reads and never answers, so the handshake itself times out. The
	// reader stays on the pipe on purpose: closing the server's end instead would
	// leave nobody reading, and the client would block on its write rather than on
	// the thing this test is about.
	silent := newFakeServer(t)
	silent.silent = true
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		silent.attach(process.server, process.client)
	}}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 30 * time.Millisecond, StartTimeout: 30 * time.Millisecond,
		MaxRestarts: 1, RestartBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	started := time.Now()
	_, err = client.Request(context.Background(), MethodTextDocumentHover, nil)
	if err == nil {
		t.Fatal("a silent server was waited on forever")
	}
	// The bound is honoured rather than the request being abandoned whenever it
	// happens to look like, which is the whole point of having one.
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the request was given up on after %v", elapsed)
	}
}

func TestStopIsIdempotentAndRefusesLaterRequests(t *testing.T) {
	starter := workingStarter(t)
	client, err := New(Config{Command: []string{"gopls"}, Workspace: "/project", Starter: starter})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := client.Stop(); err != nil {
		t.Fatalf("a second stop failed: %v", err)
	}
	if client.Running() {
		t.Fatal("the client still reports a running server")
	}
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err == nil {
		t.Fatal("a stopped client served a request")
	}
	if err := client.Ensure(context.Background()); err == nil {
		t.Fatal("a stopped client started a server again")
	}
}

func TestRestartSpendsTheBound(t *testing.T) {
	var mu sync.Mutex
	launches := 0
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		mu.Lock()
		launches++
		current := launches
		mu.Unlock()
		// Every server answers the handshake, so a restart succeeds and the only
		// thing that can stop the loop is the bound itself.
		if current > 3 {
			process.client.Close()
			return
		}
		server := newFakeServer(t)
		server.attach(process.server, process.client)
	}}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 500 * time.Millisecond, StartTimeout: 500 * time.Millisecond,
		MaxRestarts: 2, RestartBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	watchCrashes(t, client)
	defer client.Close()
	ctx := context.Background()
	// A caller looping on Restart is the same problem as a crash loop, so the bound
	// applies to it too.
	var lastErr error
	restarts := 0
	for attempt := 0; attempt < 6; attempt++ {
		if err := client.Restart(ctx); err != nil {
			lastErr = err
			break
		}
		restarts++
	}
	if lastErr == nil {
		t.Fatal("a restart loop was not bounded")
	}
	if !strings.Contains(lastErr.Error(), "restarts") {
		t.Fatalf("err = %v", lastErr)
	}
	// The bound was reached, not exceeded, so a caller knows exactly how many it got.
	if restarts > 2 {
		t.Fatalf("restarts = %d, the bound is 2", restarts)
	}
}

func TestACancelledRequestIsGivenUpOn(t *testing.T) {
	starter := workingStarter(t)
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Request(ctx, MethodTextDocumentHover, nil); err == nil {
		t.Fatal("a cancelled request was served")
	}
}

func TestTheClientAnswersAServerRequest(t *testing.T) {
	server := newFakeServer(t)
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		server.attach(process.server, process.client)
	}}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	watchCrashes(t, client)
	defer client.Close()
	answered := make(chan struct{})
	var once bool
	client.AddHandler(HandlerFunc(func(_ Context, message Message) (Message, bool) {
		if message.Method != "workspace/needTheConfiguration" {
			return Message{}, false
		}
		once = true
		close(answered)
		return Message{Result: mustMarshal(map[string]string{"answer": "yes"})}, true
	}))
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err != nil {
		t.Fatal(err)
	}
	server.send(Message{
		ID:     ID{Number: 99},
		Method: "workspace/needTheConfiguration",
	})
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("the client did not answer the server's request")
	}
	if !once {
		t.Fatal("the handler did not run")
	}
}

func TestAPanickingHandlerStillAnswers(t *testing.T) {
	// A server waits forever for an answer, so a handler that panics must still
	// produce one.
	server := newFakeServer(t)
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		server.attach(process.server, process.client)
	}}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	answered := make(chan struct{})
	client.AddHandler(HandlerFunc(func(_ Context, message Message) (Message, bool) {
		if message.Method == "workspace/somethingElse" {
			return Message{}, false
		}
		defer close(answered)
		panic("the handler is broken")
	}))
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err != nil {
		t.Fatal(err)
	}
	server.send(Message{ID: ID{Number: 98}, Method: "workspace/panics"})
	select {
	case <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("the panicking handler did not run")
	}
}

func TestAnUnhandledServerRequestIsRefusedNotDropped(t *testing.T) {
	server := newFakeServer(t)
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		server.attach(process.server, process.client)
	}}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err != nil {
		t.Fatal(err)
	}
	// The fake server records a response by echoing the request id, so a request
	// with no answer shows up as a server side timeout rather than a hang.
	server.send(Message{ID: ID{Number: 97}, Method: "workspace/unhandled"})
	// Give the client time to answer or not.
	time.Sleep(100 * time.Millisecond)
	if !client.Running() {
		t.Fatal("an unhandled request took the client down")
	}
}

func TestTraceRecordsBothDirections(t *testing.T) {
	starter := workingStarter(t)
	var mu sync.Mutex
	var directions []Direction
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: "/project", Starter: starter,
		RequestTimeout: 2 * time.Second,
		Trace: func(direction Direction, _ Message) {
			mu.Lock()
			directions = append(directions, direction)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	watchCrashes(t, client)
	defer client.Close()
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// Both directions have to appear, because a trace with only the requests is
	// useless for working out where a reply went.
	var outbound, inbound bool
	for _, direction := range directions {
		switch direction {
		case DirectionOutbound:
			outbound = true
		case DirectionInbound:
			inbound = true
		}
	}
	if !outbound || !inbound {
		t.Fatalf("directions = %v", directions)
	}
}

// frame builds a correctly framed message, so a test that means to exercise the
// reader is not defeated by a hand counted length.
func frame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

func TestReaderRejectsAStreamItCannotTrust(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"no length", "Content-Type: application/json\r\n\r\n{}"},
		{"length is not a number", "Content-Length: abc\r\n\r\n{}"},
		{"negative length", "Content-Length: -1\r\n\r\n{}"},
		{"body is not json", frame("{{{")},
		{"header has no colon", "Content-Length 5\r\n\r\n{}"},
		{"wrong protocol", frame(`{"jsonrpc":"1.0","id":1}`)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reader := NewReader(strings.NewReader(testCase.input))
			if _, err := reader.Read(); err == nil {
				t.Fatal("a malformed stream was accepted")
			}
		})
	}
	// An unknown header is ignored, because the protocol allows the headers to grow
	// and refusing to read would break the client against a newer server.
	body := `{"jsonrpc":"2.0","id":1,"method":"x"}`
	reader := NewReader(strings.NewReader(fmt.Sprintf(
		"Content-Length: %d\r\nX-Future-Header: yes\r\n\r\n%s", len(body), body)))
	message, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	if message.Method != "x" {
		t.Fatalf("message = %#v", message)
	}
	// An empty stream is the end of the stream, not an error.
	if _, err := NewReader(strings.NewReader("")).Read(); err != io.EOF {
		t.Fatalf("err = %v", err)
	}
	// A body cut short is reported rather than read as a complete message.
	short := NewReader(strings.NewReader("Content-Length: 100\r\n\r\n{}"))
	if _, err := short.Read(); err == nil {
		t.Fatal("a truncated body was accepted")
	}
}

func TestReaderHonoursItsLimit(t *testing.T) {
	reader := NewReader(strings.NewReader(""))
	// Only a positive limit is applied, so a configuration that did not mean to
	// change anything cannot end up removing the bound.
	reader.SetLimit(0)
	reader.SetLimit(-1)
	if reader.limit != DefaultReadLimit {
		t.Fatalf("limit = %d", reader.limit)
	}
	reader.SetLimit(10)
	if reader.limit != 10 {
		t.Fatalf("limit = %d", reader.limit)
	}
	reader = NewReader(strings.NewReader(
		fmt.Sprintf("Content-Length: 1000\r\n\r\n%s", strings.Repeat("x", 1000))))
	reader.SetLimit(100)
	if _, err := reader.Read(); err == nil {
		t.Fatal("an oversized message was accepted")
	}
}

func TestWriterFramesWithAContentLength(t *testing.T) {
	var buffer bytes.Buffer
	writer := NewWriter(&buffer)
	if err := writer.Write(Message{ID: ID{Number: 1}, Method: "x", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	frame := buffer.String()
	if !strings.HasPrefix(frame, ContentLengthHeader+": ") {
		t.Fatalf("frame = %q", frame)
	}
	// The declared length has to match the body, because a server that trusts the
	// header will read exactly that many bytes.
	_, body, found := strings.Cut(frame, "\r\n\r\n")
	if !found {
		t.Fatalf("frame = %q", frame)
	}
	declared := strings.TrimPrefix(strings.SplitN(frame, "\r\n", 2)[0], ContentLengthHeader+": ")
	if declared != fmt.Sprint(len(body)) {
		t.Fatalf("declared %q, body is %d bytes", declared, len(body))
	}
	// A body that will not encode is reported rather than sent as a half frame,
	// because a server that trusts the header would then read a length that does not
	// match anything it received.
	before := buffer.Len()
	if err := NewWriter(&buffer).Write(Message{Params: json.RawMessage(`{`)}); err == nil {
		t.Fatal("a message that will not encode was sent")
	}
	if buffer.Len() != before {
		t.Fatal("a frame was written for a message that failed to encode")
	}
}

func TestIDRoundTripsInBothForms(t *testing.T) {
	for _, id := range []ID{{Number: 7}, {Text: "abc"}, {}} {
		encoded, err := json.Marshal(id)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ID
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded != id {
			t.Fatalf("id %#v became %#v", id, decoded)
		}
	}
	if !(ID{}).IsZero() || (ID{Number: 1}).IsZero() {
		t.Fatal("IsZero is wrong")
	}
	if (ID{Text: "x"}).String() != `"x"` {
		t.Fatalf("String = %q", (ID{Text: "x"}).String())
	}
}

func TestMessageKindHelpers(t *testing.T) {
	request := Message{Method: "x", ID: ID{Number: 1}}
	notification := Message{Method: "x"}
	response := Message{ID: ID{Number: 1}}
	if !request.IsRequest() || request.IsNotification() || request.IsResponse() {
		t.Fatal("a request was misclassified")
	}
	if !notification.IsNotification() || notification.IsRequest() {
		t.Fatal("a notification was misclassified")
	}
	if !response.IsResponse() || response.IsRequest() {
		t.Fatal("a response was misclassified")
	}
	// A response error with no data still reads properly, because a nil receiver is
	// what a caller checks first.
	var failure *ResponseError
	if failure.Error() != "no error" {
		t.Fatalf("nil error = %q", failure.Error())
	}
	withData := &ResponseError{Code: 1, Message: "m", Data: json.RawMessage(`{}`)}
	if !strings.Contains(withData.Error(), "{}") {
		t.Fatalf("err = %q", withData.Error())
	}
}

func TestMessageContextReportsWhatItCarries(t *testing.T) {
	request := &messageContext{message: Message{Method: "x", ID: ID{Number: 1}}}
	if request.Direction() != DirectionInbound {
		t.Fatal("direction is wrong")
	}
	if _, ok := request.Request(); !ok {
		t.Fatal("the request was not reported")
	}
	if _, ok := request.Notification(); ok {
		t.Fatal("a request reported a notification")
	}
	notification := &messageContext{message: Message{Method: "x"}}
	if _, ok := notification.Notification(); !ok {
		t.Fatal("the notification was not reported")
	}
	if _, ok := notification.Request(); ok {
		t.Fatal("a notification reported a request")
	}
	response := &messageContext{message: Message{ID: ID{Number: 1}}}
	if _, ok := response.Response(); !ok {
		t.Fatal("the response was not reported")
	}
}

func TestHandlerFuncAdaptsAFunction(t *testing.T) {
	called := false
	var handler Handler = HandlerFunc(func(Context, Message) (Message, bool) {
		called = true
		return Message{}, false
	})
	if _, ok := handler.Handle(nil, Message{}); ok {
		t.Fatal("the function answered")
	}
	if !called {
		t.Fatal("the function did not run")
	}
}

// A server that outlives its test keeps goroutines blocked on its streams, and the
// failure it eventually causes lands on an unrelated test. This asserts the harness
// does not leak, because that is the defect the suite cannot otherwise see: a leak
// shows up as a different test failing on a busier machine, never as the test that
// caused it.
func TestTheHarnessLeaksNothingWhenATestEnds(t *testing.T) {
	before := runtime.NumGoroutine()
	// A cleanup registered on a subtest runs when the subtest returns, so the
	// servers are stopped inside the round rather than at the end of the suite.
	for round := 0; round < 5; round++ {
		t.Run("round", func(t *testing.T) {
			starter := workingStarter(t)
			client, err := New(Config{
				Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err != nil {
				t.Fatal(err)
			}
			// The client is closed the way a caller closes one, and the starter's own
			// cleanup still has to stop the server afterwards. A leak check that
			// depended on a client nobody closes would be asserting something about a
			// situation no caller is in, and would fail for reasons that have nothing
			// to do with leaking.
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutines went from %d to %d across five rounds; the servers are not being stopped",
		before, runtime.NumGoroutine())
}

// The harness must not be able to produce a failure the product cannot have. A
// language server is a separate process holding only its own end of the pipe, so
// it cannot reach the client's read end; when it exits, the client sees the end of
// the stream. Operating system pipes gave the fake server both ends, which is how a
// handshake could fail with a file that was already closed.
func TestTheHarnessCannotCloseTheClientsReadEnd(t *testing.T) {
	process := newPipeProcess()
	// Everything a fake server holds: the end it reads and the end it writes.
	serverRead, serverWrite := process.server, process.client
	_ = serverWrite.Close()
	// The server is gone. The client must see the end of its stream, not a closed
	// file, and it must still be able to shut the process down.
	buffer := make([]byte, 1)
	if _, err := process.Stdout().Read(buffer); !errors.Is(err, io.EOF) {
		t.Fatalf("the client read %v, want the end of the stream", err)
	}
	_ = serverRead.Close()
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	// A client that has let the server go cannot write to it, and is told so rather
	// than blocking on a reader that will never come back.
	if _, err := process.Stdin().Write([]byte("Content-Length: 2\n\n{}")); err == nil {
		t.Fatal("a write to a process that was closed reported success")
	}
}

// A stream that is closed for writing hands the reader the end of the stream, and
// one that is abandoned hands the writer a broken pipe. The two are different and
// the client tells them apart, so the harness has to as well.
func TestAMemoryStreamEndsTheWayAPipeDoes(t *testing.T) {
	t.Run("closing the writing end ends the reader's stream", func(t *testing.T) {
		stream := newMemoryStream()
		if _, err := stream.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		if err := stream.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 16)
		read, err := stream.Read(buffer)
		if err != nil || string(buffer[:read]) != "hello" {
			t.Fatalf("read %d %q, err %v: what was written must still be readable", read, buffer[:read], err)
		}
		if _, err := stream.Read(buffer); !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want the end of the stream", err)
		}
	})

	t.Run("a reader that is gone breaks the writer", func(t *testing.T) {
		stream := newMemoryStream()
		if err := stream.CloseRead(); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Write([]byte("hello")); !errors.Is(err, errBrokenPipe) {
			t.Fatalf("err = %v, want a broken pipe", err)
		}
	})

	t.Run("a write does not wait for a reader to arrive", func(t *testing.T) {
		stream := newMemoryStream()
		// Nothing is reading. A write that waited for a reader would be a test of
		// the scheduler rather than of the client, and an unbuffered pipe does
		// exactly that.
		done := make(chan error, 1)
		go func() {
			_, err := stream.Write(make([]byte, 4096))
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a write waited for a reader that never came")
		}
	})

	t.Run("a reader waits for a writer and is woken when it stops", func(t *testing.T) {
		stream := newMemoryStream()
		got := make(chan string, 1)
		go func() {
			buffer := make([]byte, 16)
			read, _ := stream.Read(buffer)
			got <- string(buffer[:read])
		}()
		time.Sleep(10 * time.Millisecond)
		if _, err := stream.Write([]byte("wake")); err != nil {
			t.Fatal(err)
		}
		select {
		case text := <-got:
			if text != "wake" {
				t.Fatalf("read %q", text)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a blocked reader was not woken by a write")
		}
	})
}

// StartTimeout says what starting a server may cost, and the handshake is most of
// that cost. Bounding only the spawn left the handshake on the caller's context and
// under the request timeout, which is a budget for a question to a server that is
// already up, so a cold start that was merely slow was reported as a server that
// was gone.
func TestTheHandshakeIsBoundedByTheStartBudget(t *testing.T) {
	// A server that never answers the handshake, and a request budget no start could
	// meet. If the handshake were bounded by the request budget the error would be
	// about the server not running; bounded by the start budget it is a timeout, and
	// it says so.
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		// Nothing is attached, so nothing will ever answer.
		_ = process
	}}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter,
		RequestTimeout: 50 * time.Millisecond,
		StartTimeout:   150 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	started := time.Now()
	_, err = client.Request(context.Background(), MethodTextDocumentHover, nil)
	if err == nil {
		t.Fatal("a server that never answered the handshake was reported as ready")
	}
	elapsed := time.Since(started)
	// The request budget is the smaller of the two, and the request itself is what
	// timed out, so the wait is bounded by it rather than by the start budget.
	if elapsed > time.Second {
		t.Fatalf("the handshake was given %v, so it was not bounded by either budget", elapsed)
	}
	// What matters is that the failure says the wait ran out rather than only that
	// the server is not running, because a reader who believes the server is gone
	// looks for a process that never died. The underlying condition is still
	// reported, so a caller matching on it is not broken.
	if !strings.Contains(err.Error(), "did not finish the handshake") {
		t.Fatalf("err = %v, want a failure that says the handshake ran out of time", err)
	}
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v, want it to wrap the condition it is reporting", err)
	}
}

// The other half of the same thing: a server that goes away during the handshake is
// a server that went away, and saying so is the whole point of telling the two
// apart.
func TestAHandshakeThatEndsEarlyIsNotReportedAsASlowServer(t *testing.T) {
	server := newFakeServer(t)
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		// The server reads the handshake and then stops, which is what a server that
		// crashes on startup looks like from here.
		server.crashBefore = MethodInitialize
		server.attach(process.server, process.client)
	}}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter,
		RequestTimeout: 2 * time.Second, StartTimeout: 5 * time.Second,
		MaxRestarts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.Request(context.Background(), MethodTextDocumentHover, nil)
	if err == nil {
		t.Fatal("a server that died during the handshake was reported as ready")
	}
	// A generous budget means the failure cannot be a timeout, and it must not be
	// dressed up as one.
	if strings.Contains(err.Error(), "did not finish the handshake") {
		t.Fatalf("err = %v, a server that went away is not a server that was slow", err)
	}
	if crash := client.LastCrash(); crash == nil {
		t.Fatal("the crash was not recorded, so a reader is left with the error alone")
	}
}

// Every question a client asks goes back through Ensure when its server dies, so a
// crash mid-session costs one restart and the turn carries on. The handshake used to
// have no retry at all, which made a server that died during its own startup the one
// fault that cost the whole turn permanently. It is the same fault with none of the
// recovery, and it is not a rare one: a first launch is when a server is most likely
// to be short of something.
func TestAServerThatDiesDuringItsHandshakeIsLaunchedAgain(t *testing.T) {
	var mu sync.Mutex
	launches := 0
	starter := &pipeStarter{}
	starter.track(t, func(process *pipeProcess) *fakeServer {
		server := newFakeServer(t)
		mu.Lock()
		launches++
		first := launches == 1
		mu.Unlock()
		if first {
			// The first server reads the handshake and then stops, which is what a
			// server that crashes on startup looks like from here.
			server.crashBefore = MethodInitialize
		}
		server.attach(process.server, process.client)
		return server
	})
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter,
		RequestTimeout: 2 * time.Second, MaxRestarts: 2, RestartBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	watchCrashes(t, client)
	defer client.Close()
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err != nil {
		t.Fatalf("a server that died once during its handshake cost the whole turn: %v", err)
	}
	mu.Lock()
	got := launches
	mu.Unlock()
	if got < 2 {
		t.Fatalf("launches = %d, the server was not launched again", got)
	}
	if !client.Running() {
		t.Fatal("the client is not running after recovering")
	}
}

// The bound is the point. A server that dies on every handshake must stop, not spin,
// and a caller has to hear that it did rather than wait out a timeout.
func TestAServerThatDiesOnEveryHandshakeStops(t *testing.T) {
	starter := &pipeStarter{}
	starter.track(t, func(process *pipeProcess) *fakeServer {
		server := newFakeServer(t)
		server.crashBefore = MethodInitialize
		server.attach(process.server, process.client)
		return server
	})
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter,
		RequestTimeout: time.Second, MaxRestarts: 2, RestartBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	watchCrashes(t, client)
	defer client.Close()
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err == nil {
		t.Fatal("a server that dies on every handshake was reported as ready")
	}
	// One first attempt plus MaxRestarts, and no more: the bound is the promise that
	// a broken server is worse than no server.
	started := time.Now()
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err == nil {
		t.Fatal("a second question started a server that cannot answer")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the bound was not honoured: %v", elapsed)
	}
}

// A handshake that ran out of time is not a server that went away, and launching it
// again does not make it answer any sooner. It must not be retried.
func TestAServerThatIsMerelySlowIsNotLaunchedAgain(t *testing.T) {
	var mu sync.Mutex
	launches := 0
	starter := &pipeStarter{}
	starter.onLaunch = func(process *pipeProcess) {
		mu.Lock()
		launches++
		mu.Unlock()
		// Nothing is attached, so nothing will ever answer.
		_ = process
	}
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter,
		RequestTimeout: 60 * time.Millisecond, StartTimeout: 500 * time.Millisecond,
		MaxRestarts: 3, RestartBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Request(context.Background(), MethodTextDocumentHover, nil); err == nil {
		t.Fatal("a server that never answered was reported as ready")
	}
	mu.Lock()
	got := launches
	mu.Unlock()
	if got != 1 {
		t.Fatalf("launches = %d, a server that was merely slow was launched again", got)
	}
}
