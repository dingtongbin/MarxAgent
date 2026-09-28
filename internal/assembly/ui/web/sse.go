// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event is one thing that happened, with the position it happened at.
//
// The sequence is part of the event rather than something the transport adds,
// because it comes from the journal and the journal is what a reconnecting window
// will be told to resume from. A position the transport invented would be a position
// that means nothing after a restart.
type Event struct {
	// Seq is the per stream position, monotonic and assigned by the journal.
	Seq int64 `json:"seq"`
	// Payload is the event as it was published.
	Payload json.RawMessage `json:"payload"`
}

// Stream is where the event stream comes from.
//
// It is an interface so this package does not depend on the core event bus. The
// assembly layer supplies the real one, which is backed by the journal; a test
// supplies a fixed one. What matters here is only that events arrive in order, that
// each carries its position, and that a subscriber can stop.
type Stream interface {
	// Subscribe returns the events for a session that come after the given position,
	// and a function that ends the subscription. The function is safe to call more
	// than once, because a caller that has been interrupted and a caller that has
	// finished both end up calling it.
	//
	// A position of zero means from the earliest event still held. A position past
	// the end means there is nothing to send and the channel closes, which is the
	// answer a window that is already up to date needs.
	Subscribe(ctx context.Context, sessionID string, after int64) (<-chan Event, func(), error)
}

// sseHandler serves the event stream as Server-Sent Events.
//
// Server-Sent Events rather than a WebSocket, and the reason is one feature: a
// reconnecting browser sends back the id of the last event it received, and the
// server can resume from there. The journal already has every event with a monotonic
// position, so a window that was closed for ten seconds asks for what it missed and
// gets exactly that.
//
// A WebSocket gives none of this. It would mean a reconnect with backoff written by
// hand, a resume mechanism invented alongside it, and a heartbeat to notice a dead
// peer, all to arrive at the same place with more to go wrong.
type sseHandler struct {
	stream Stream
	// closing is closed when the server is stopping. The handler watches it so a
	// stream ends on a stop rather than being waited for, which is the difference
	// between a program that closes in a moment and one that hangs until it is killed.
	closing <-chan struct{}
	// mu guards the counters below.
	mu sync.Mutex
	// windows is how many streams are open, so the multi-window case can be counted
	// rather than claimed.
	windows int
	// resumers is how many reconnects have asked to resume. A number that stays at
	// zero would mean the feature nobody uses, and a number that is always every
	// connection would mean nobody is watching the first time through.
	resumers int
}

// newSSEHandler builds the stream handler.
func newSSEHandler(stream Stream, closing <-chan struct{}) *sseHandler {
	return &sseHandler{stream: stream, closing: closing}
}

// sseWriteTimeout bounds one flush.
//
// It is short because this goes to a browser tab on this machine, where a peer that
// has stopped reading is a closed tab rather than a slow network. Waiting minutes for
// one would hold a goroutine that is also serving every other window.
const sseWriteTimeout = 10 * time.Second

// sseKeepAlive is how often a comment is sent when nothing has happened.
//
// A comment rather than an event, because it keeps the connection and any proxy in
// front of it alive without appearing in the window's event list. A model thinking
// for a minute produces nothing, and a connection that has said nothing for a minute
// is indistinguishable from a dead one.
const sseKeepAlive = 20 * time.Second

// sseRetry is what the window is told to wait before reconnecting.
//
// It is a second rather than nothing, because the commonest cause of a dropped stream
// is a server that is restarting and will be back shortly, and reconnecting in a tight
// loop against a server that is not coming back is how a laptop's battery goes.
const sseRetry = 2 * time.Second

// serve writes the stream to one window.
func (h *sseHandler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "a stream is opened with GET", http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("session"))
	if sessionID == "" {
		// A stream with no session would be a stream carrying everything, and a window
		// watching everything cannot be reasoned about when it is three windows
		// watching three different sessions.
		http.Error(w, "name the session to watch", http.StatusBadRequest)
		return
	}

	// The position the window last saw. It comes from the header the browser sets on
	// its own reconnect, so a window that has been closed since yesterday still asks
	// for yesterday's events rather than starting from nothing.
	after, resumed := h.resumeFrom(r)

	events, stop, err := h.stream.Subscribe(r.Context(), sessionID, after)
	if err != nil {
		http.Error(w, "the event stream is not available: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer stop()

	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without a flush the bytes sit in a buffer and the window shows nothing at
		// all, with no error anywhere. Saying so is better than an open connection
		// that is silently doing nothing.
		http.Error(w, "this server cannot stream", http.StatusInternalServerError)
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	// Nginx and the like buffer a proxied response by default, which turns a stream
	// into a download. This header is the standard way of asking them not to.
	header.Set("X-Accel-Buffering", "no")
	header.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// The retry hint goes out before anything else, so a window that cannot connect
	// at all still learns how patient to be.
	fmt.Fprintf(w, "retry: %d\n\n", sseRetry.Milliseconds())
	h.countWindow(1, resumed)
	defer h.countWindow(-1, false)
	flusher.Flush()

	keepAlive := time.NewTicker(sseKeepAlive)
	defer keepAlive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.closing:
			// The server is stopping. Ending here rather than being waited for is what
			// makes a stop take the time it should instead of the whole budget.
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, open := <-events:
			if !open {
				return
			}
			// The id is what makes the resume work, so it is written first and in the
			// frame the specification says it goes in. An event with no id is an event
			// a reconnecting window will ask for again.
			if _, err := fmt.Fprintf(w, "id: %d\n", event.Seq); err != nil {
				return
			}
			for _, line := range strings.Split(string(event.Payload), "\n") {
				// A payload with a newline in it would otherwise look like the end of
				// the frame, and the rest of it would be parsed as a field.
				fmt.Fprintf(w, "data: %s\n", line)
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// resumeFrom reads where the window wants to carry on from.
//
// It is asked for rather than assumed, because a window that has just opened and a
// window that has been closed for an hour are both first requests from the server's
// point of view, and treating them the same would make the second one replay a day of
// events into a panel nobody is watching.
func (h *sseHandler) resumeFrom(r *http.Request) (int64, bool) {
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0, false
	}
	position, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || position < 0 {
		// A header that is not a position is ignored rather than guessed at. Guessing
		// would mean either replaying everything or skipping a gap, and both are worse
		// than starting again from a known point.
		return 0, false
	}
	return position, true
}

// countWindow keeps the counters that make the stream observable.
//
// The two are counted together on purpose: a reconnect that is never counted is a
// resume mechanism nobody has evidence works, and a window count that never falls
// would mean a stream that cannot be closed.
func (h *sseHandler) countWindow(delta int, resumed bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.windows += delta
	if h.windows < 0 {
		h.windows = 0
	}
	if resumed {
		h.resumers++
	}
}

// Windows reports how many streams are open right now.
func (h *sseHandler) Windows() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.windows
}

// Resumers reports how many connections have asked to carry on from a position.
func (h *sseHandler) Resumers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resumers
}
