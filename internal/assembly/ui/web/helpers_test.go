// SPDX-License-Identifier: Apache-2.0

package web

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestServer builds a server with a token and a stream, starts it, and stops it
// when the test ends.
//
// It starts it because a listener with nothing accepting on it is the worst of both:
// the kernel completes the connection and then nothing answers, so a client waits for
// ever and a test that meant to check a stream hangs instead. The tests that drive the
// handler directly do not need this and are given the same server, because a handler
// can be called without a socket and a socket should not be opened for nothing.
func newTestServer(t *testing.T, stream *testStream) (*Server, *testStream) {
	t.Helper()
	if stream == nil {
		stream = &testStream{}
	}
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := LoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Listener: listener, Token: token, Stream: stream})
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Start(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("the server stopped with %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the server did not stop within the budget")
		}
	})
	return server, stream
}

// testListener makes a listener for the cases that only need somewhere to be.
func testListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := LoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// authorised adds the token the way a caller who has it would.
func authorised(request *http.Request, token string) *http.Request {
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

// testStream hands out a fixed set of events and remembers what it was asked for, so
// a test can see that a window carried on from where it said it had got to.
type testStream struct {
	mu       sync.Mutex
	events   []Event
	after    int64
	askedFor string
	// closeOnce guards the done channel. A subscription ends by its context being
	// cancelled and a test can also end it directly, so there are two ways to close
	// and one of them has to be the only one that actually closes.
	closeOnce sync.Once
	done      chan struct{}
}

func (s *testStream) Subscribe(ctx context.Context, sessionID string, after int64) (<-chan Event, func(), error) {
	s.mu.Lock()
	s.after = after
	s.askedFor = sessionID
	s.done = make(chan struct{})
	s.mu.Unlock()

	out := make(chan Event)
	// The events are offered from a goroutine because the handler reads until the
	// channel closes, and a handler that runs on the caller's goroutine would block
	// the subscription from ever being set up.
	go func() {
		defer close(out)
		for _, event := range s.events {
			if event.Seq <= after {
				// This is the resume working: an event the window has already seen is
				// not sent again.
				continue
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			case <-s.done:
				return
			}
		}
		// The channel is deliberately left open. A stream that ends looks to a window
		// like a server that has gone, and a stream with nothing to say yet has not
		// gone anywhere.
		<-ctx.Done()
	}()

	stop := func() { s.end() }
	return out, stop, nil
}

// end closes the subscription once, whoever gets there first.
func (s *testStream) end() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		done := s.done
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
	})
}

// resumeFrom reports what a window asked to carry on from.
func (s *testStream) resumeFrom() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.after
}

// session reports which session a window asked about.
func (s *testStream) session() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.askedFor
}

// close ends the events, for a test that wants the stream to finish.
func (s *testStream) close() { s.end() }

// streamConn is one window's side of a stream, read as raw text.
//
// It reads the wire rather than parsing events, because what is being checked is what
// was written. A parser on this side would agree with a writer that is wrong in the
// same way, and two wrongs agreeing is not evidence.
type streamConn struct {
	body    io.ReadCloser
	reader  *bufio.Reader
	request *http.Request
}

func (c *streamConn) Close() {
	if c.body != nil {
		c.body.Close()
	}
	if c.request != nil {
		c.request.Context()
	}
}

// await reads frames until one carries an event, and returns it whole.
func (c *streamConn) await(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var frame strings.Builder
	for time.Now().Before(deadline) {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("reading the stream: %v (read so far: %q)", err, frame.String())
		}
		frame.WriteString(line)
		// A frame ends at a blank line, and the frames before it may be only the
		// retry hint and a keepalive.
		if line == "\n" && strings.Contains(frame.String(), "data: ") {
			return frame.String()
		}
	}
	t.Fatalf("no event arrived; read so far: %q", frame.String())
	return ""
}

// openStream opens one window on a session and waits for the response headers.
//
// The last event id is sent the way a browser sends it, which is as a header and not
// as a parameter, because that is the only way the server can tell a first request
// from a reconnect.
func openStream(t *testing.T, server *Server, token, lastEventID string, after int64) *streamConn {
	t.Helper()
	url := fmt.Sprintf("http://%s/events?session=test", server.Addr())
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	_ = after

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("the stream answered %d: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		response.Body.Close()
		t.Fatalf("the stream is %q", got)
	}
	return &streamConn{body: response.Body, reader: bufio.NewReader(response.Body), request: request}
}
