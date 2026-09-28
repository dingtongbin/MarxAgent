// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/assembly/ui/web"
	"github.com/dingtongbin/MarxAgent/internal/core"
)

// The replay bound. It is the number of events one conversation keeps in memory
// for a window that reconnects.
//
// It exists because a position that means something has to be resumable, and a
// journal that has been closed is not. A window that was closed for longer than
// this bound gets told so and reloads the conversation over the API instead,
// which is slower and correct; a window that gets a gap in the middle of its
// stream without being told would render a conversation that is missing a
// message, and nothing about that looks wrong.
const hubReplayBound = 512

// hub is the event stream: one sequence per conversation, several windows
// watching each.
//
// The position belongs to the conversation rather than to a window, which is what
// makes a browser reconnect meaningful: the window says where it got to, and the
// hub says what happened since. A position invented per connection would mean a
// reconnect replays everything or skips something, and there is no third answer.
type hub struct {
	mu sync.Mutex
	// sessions holds the retained tail of each conversation's events.
	sessions map[string]*hubSession
	// closed ends every subscription when the runtime shuts down, so a window's
	// stream finishes rather than hanging on a channel nobody will write to.
	closed  bool
	closing chan struct{}
}

type hubSession struct {
	// events is the retained tail, oldest first. seq is the position of the last
	// event in it.
	events []web.Event
	seq    int64
	// watchers are the open streams, each with the position it has reached.
	watchers map[*watcher]struct{}
}

type watcher struct {
	// send is the window's channel. It is buffered, because a window that stops
	// reading for a moment is a window that is still open.
	send chan web.Event
	// closed guards the close, because a subscription has more than one way to
	// end and two of them can race.
	closed bool
}

func newHub() *hub {
	return &hub{sessions: make(map[string]*hubSession), closing: make(chan struct{})}
}

// publish puts one event on every window watching a conversation.
func (h *hub) publish(sessionID string, event core.Event) {
	payload, err := encodeEvent(event)
	if err != nil {
		return
	}
	h.deliver(sessionID, payload)
}

// publishState tells the windows about the conversation rather than about
// something that happened in it.
//
// A window's task menu is wrong until it is told, and no core event carries the
// conversation's own state, so this is its own path. It carries the same
// position space as everything else, which is what lets a reconnecting window
// receive the state it missed rather than only the events.
func (h *hub) publishState(info SessionInfo) {
	payload, err := json.Marshal(map[string]any{
		"type":    "session",
		"session": info,
	})
	if err != nil {
		return
	}
	h.deliver(info.ID, payload)
}

// deliver assigns a position to one event and hands it to every watcher.
//
// A watcher that will not take an event is dropped rather than waited for. The
// hub is the only thing that can give a window an event, so a slow window that
// were waited for would stop the conversation for every other window watching it
// as well; and a window that has fallen this far behind is going to be told it
// missed something, reload, and carry on. Losing a turn's events to a window that
// was not reading is recoverable; blocking the turn for every other window
// because of one is not.
func (h *hub) deliver(sessionID string, payload json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	session := h.sessionLocked(sessionID)
	session.seq++
	framed := web.Event{Seq: session.seq, Payload: payload}
	session.events = append(session.events, framed)
	if len(session.events) > hubReplayBound {
		session.events = session.events[len(session.events)-hubReplayBound:]
	}
	for watcher := range session.watchers {
		select {
		case watcher.send <- framed:
		default:
			// The window is not reading. Its stream ends; the browser reconnects
			// with the position it last saw and is told about the gap.
			h.unsubscribeLocked(session, watcher)
		}
	}
}

// Subscribe implements web.Stream.
func (h *hub) Subscribe(ctx context.Context, sessionID string, after int64) (<-chan web.Event, func(), error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, ErrClosed
	}
	session := h.sessionLocked(sessionID)
	watcher := &watcher{send: make(chan web.Event, hubWatchBuffer)}
	session.watchers[watcher] = struct{}{}
	var replay []web.Event
	switch {
	case after > 0 && session.seq == 0:
		// The window was watching a conversation that has been replaced, or this
		// is a different process. There is nothing to resume from and nothing
		// would arrive, so the window is told to reload rather than left watching
		// a stream that will never speak.
		replay = []web.Event{notice("reset", "this conversation has no events here; reload it")}
	case after > session.seq:
		// A position from beyond the end, which is what a window holding a
		// position from another server looks like. Refusing to invent events to
		// fill the gap is the only honest answer, and a reset says so in a way the
		// window can act on.
		replay = []web.Event{notice("reset", "the position this window held is past the end; reload the session")}
	case after > 0:
		for _, event := range session.events {
			if event.Seq > after {
				replay = append(replay, event)
			}
		}
		if len(replay) == 0 && after < session.seq-hubReplayBound {
			// The events it asked for are the ones that were dropped from the
			// tail. Rather than a stream that starts in the middle, the window is
			// told to reload.
			replay = []web.Event{notice("reset", "the events from that position are no longer held; reload the session")}
		}
	default:
		// A window that has just opened is sent nothing it does not need: the
		// conversation is loaded over the API and the stream carries what happens
		// from here.
		replay = []web.Event{notice("ready", "")}
	}
	h.mu.Unlock()

	events := make(chan web.Event, hubWatchBuffer+len(replay))
	for _, event := range replay {
		events <- event
	}
	go h.pump(ctx, session, watcher, events)

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			h.mu.Lock()
			h.unsubscribeLocked(session, watcher)
			h.mu.Unlock()
		})
	}
	return events, stop, nil
}

// hubWatchBuffer is how many events one window may be behind before its stream
// is ended. It is a few turns' worth of streaming tokens, which is more than a
// browser on a local connection takes to draw, and small enough that a window
// that was never reading does not hold the tail.
const hubWatchBuffer = 256

// notice is a control message that carries no position.
//
// A zero sequence is not an oversight: the position in a frame is what a
// browser remembers and asks to resume from, so a notice that invented one would
// leave every window holding a position that no event has, and a stream that
// resumed from there would never speak again. A window that is told to reload
// does so, and its next connection starts from the beginning with a fresh
// position.
func notice(kind, detail string) web.Event {
	payload := map[string]any{"type": kind}
	if detail != "" {
		payload["data"] = detail
	}
	return web.Event{Seq: 0, Payload: mustJSON(payload)}
}

// pump moves events from the watcher to the window, and closes the window's
// channel when the stream ends.
func (h *hub) pump(ctx context.Context, session *hubSession, source *watcher, sink chan<- web.Event) {
	defer close(sink)
	for {
		select {
		case <-ctx.Done():
			h.mu.Lock()
			h.unsubscribeLocked(session, source)
			h.mu.Unlock()
			return
		case <-h.closing:
			h.mu.Lock()
			h.unsubscribeLocked(session, source)
			h.mu.Unlock()
			return
		case event, open := <-source.send:
			if !open {
				return
			}
			select {
			case sink <- event:
			case <-ctx.Done():
				h.mu.Lock()
				h.unsubscribeLocked(session, source)
				h.mu.Unlock()
				return
			case <-h.closing:
				h.mu.Lock()
				h.unsubscribeLocked(session, source)
				h.mu.Unlock()
				return
			}
		}
	}
}

// unsubscribeLocked removes a watcher. The caller holds the lock.
func (h *hub) unsubscribeLocked(session *hubSession, watcher *watcher) {
	if _, watching := session.watchers[watcher]; !watching {
		return
	}
	delete(session.watchers, watcher)
	if !watcher.closed {
		watcher.closed = true
		close(watcher.send)
	}
}

func (h *hub) sessionLocked(sessionID string) *hubSession {
	session, exists := h.sessions[sessionID]
	if !exists {
		session = &hubSession{watchers: make(map[*watcher]struct{})}
		h.sessions[sessionID] = session
	}
	return session
}

// close ends every open stream.
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.closing)
	for _, session := range h.sessions {
		for watcher := range session.watchers {
			if !watcher.closed {
				watcher.closed = true
				close(watcher.send)
			}
			delete(session.watchers, watcher)
		}
	}
	h.sessions = make(map[string]*hubSession)
}

// Watchers reports how many windows are watching a conversation, so the
// multi-window case can be counted rather than claimed.
func (h *hub) Watchers(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	session, exists := h.sessions[sessionID]
	if !exists {
		return 0
	}
	return len(session.watchers)
}

// encodeEvent turns a core event into what a window receives.
//
// The core's own envelope carries its payload as a JSON string, because that is
// what goes into a journal record. A window would have to parse a string to get
// at an object, so the payload is carried as JSON here instead. The rest of the
// envelope is the core's, unchanged.
func encodeEvent(event core.Event) (json.RawMessage, error) {
	envelope := struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		SessionID string          `json:"session_id,omitempty"`
		AgentID   string          `json:"agent_id,omitempty"`
		Error     string          `json:"error,omitempty"`
		Data      json.RawMessage `json:"data,omitempty"`
	}{
		Type:      string(event.Type),
		SessionID: event.SessionID,
		AgentID:   event.AgentID,
		Error:     event.Error,
	}
	if !event.Timestamp.IsZero() {
		envelope.Timestamp = event.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	if event.Data != "" {
		envelope.Data = json.RawMessage(event.Data)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return encoded
}

var _ web.Stream = (*hub)(nil)
