// SPDX-License-Identifier: Apache-2.0

// Package subagent holds the sub agent infrastructure: the registry that decides
// who may create whom, the message broker that carries traffic between them, and
// the orchestration patterns that run several of them over one task.
//
// The design's rule is that only the main core may create a sub agent, and a sub
// agent may not create another. That rule is enforced here rather than left to
// convention, because a sub agent that could spawn a chain would multiply the work
// no one bounded and the audit trail no one could follow.
package subagent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Route names a destination.
const (
	// RouteBroadcast reaches every registered agent, the main core included.
	RouteBroadcast = "broadcast"
	// RouteMain addresses the main core by role rather than by identifier, so a
	// caller does not have to know the identifier it was assigned.
	RouteMain = "main"
)

// MessageType names what a message is for.
const MessageType string = "task"

const (
	// TypeTask asks a recipient to do something.
	TypeTask = "task"
	// TypeResult reports the outcome of a task.
	TypeResult = "result"
	// TypeQuery asks for information without asking for work.
	TypeQuery = "query"
	// TypeResponse answers a query.
	TypeResponse = "response"
	// TypeStatus reports a state change.
	TypeStatus = "status"
)

// Message is one message between agents.
//
// It is a value rather than a pointer and its fields are set at construction,
// because a message is shared across goroutines and a caller that could still
// mutate it after handing it over would be a data race waiting to happen.
type Message struct {
	ID          string    `json:"id"`
	FromAgentID string    `json:"from"`
	ToAgentID   string    `json:"to"`
	Type        string    `json:"type"`
	Content     string    `json:"content"`
	Data        []byte    `json:"data,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	// Sequence is the per stream position assigned by the journal, filled in by
	// the broker on send. It is what makes a replay idempotent.
	Sequence int64 `json:"sequence,omitempty"`
	// Orphan marks a message that was in the journal when the process died. It is
	// never delivered, only filed, because the recipient no longer exists.
	Orphan bool `json:"orphan,omitempty"`
}

// Clone returns a copy, so a receiver cannot reach into a sender's buffers.
func (m Message) Clone() Message {
	copied := m
	if m.Data != nil {
		copied.Data = append([]byte(nil), m.Data...)
	}
	return copied
}

// validate reports whether a message can be sent.
//
// The timestamp is not required: the broker assigns one when the caller left it
// out, because a caller that invented its own clock would produce records that
// disagree with the journal's.
func (m Message) validate() error {
	if strings.TrimSpace(m.FromAgentID) == "" {
		return fmt.Errorf("subagent: a message needs a sender")
	}
	if strings.TrimSpace(m.ToAgentID) == "" {
		return fmt.Errorf("subagent: a message needs a recipient")
	}
	if strings.TrimSpace(m.Type) == "" {
		return fmt.Errorf("subagent: a message needs a type")
	}
	return nil
}

// Mailbox receives messages for one agent.
type Mailbox struct {
	agentID string
	// mailbox is buffered so a sender is not blocked by a receiver that is busy,
	// and bounded so a receiver that is gone cannot grow it without limit.
	mailbox chan Message
	// dropped counts messages this mailbox could not hold, which is a number worth
	// surfacing rather than a silent loss.
	dropped int64
	mu      sync.Mutex
	closed  bool
}

// MailboxCapacity is how many messages an agent may fall behind by before sends to
// it start being reported as dropped.
const MailboxCapacity = 64

// newMailbox builds a mailbox.
func newMailbox(agentID string) *Mailbox {
	return &Mailbox{agentID: agentID, mailbox: make(chan Message, MailboxCapacity)}
}

// Receive returns the next message, blocking until one arrives or the mailbox is
// closed.
func (m *Mailbox) Receive() (Message, bool) {
	message, ok := <-m.mailbox
	if !ok {
		return Message{}, false
	}
	return message.Clone(), true
}

// ReceiveWithin returns the next message, giving up after the wait.
func (m *Mailbox) ReceiveWithin(wait time.Duration) (Message, bool) {
	if wait <= 0 {
		select {
		case message, ok := <-m.mailbox:
			if !ok {
				return Message{}, false
			}
			return message.Clone(), true
		default:
			return Message{}, false
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case message, ok := <-m.mailbox:
		if !ok {
			return Message{}, false
		}
		return message.Clone(), true
	case <-timer.C:
		return Message{}, false
	}
}

// Len reports how many messages are waiting.
func (m *Mailbox) Len() int { return len(m.mailbox) }

// Dropped reports how many messages this mailbox could not hold.
func (m *Mailbox) Dropped() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped
}

// deliver hands a message over, reporting whether it was accepted.
//
// A full mailbox is reported rather than blocking. A sender that blocked on a
// receiver that is stuck would turn one slow agent into a stalled session, and the
// journal has already recorded the message, so nothing is lost by saying so.
func (m *Mailbox) deliver(message Message) bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	select {
	case m.mailbox <- message.Clone():
		return true
	default:
		m.mu.Lock()
		m.dropped++
		m.mu.Unlock()
		return false
	}
}

// close empties the mailbox and refuses further deliveries.
func (m *Mailbox) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()
	close(m.mailbox)
}

// encode renders a message for the journal.
func encode(message Message) ([]byte, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("subagent: encode the message: %w", err)
	}
	return encoded, nil
}

// decode parses a message from the journal.
func decode(data []byte) (Message, error) {
	var message Message
	if err := json.Unmarshal(data, &message); err != nil {
		return Message{}, fmt.Errorf("subagent: decode the message: %w", err)
	}
	return message, nil
}
