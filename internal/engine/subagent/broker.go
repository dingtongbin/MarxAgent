// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// ErrNotMain is returned when something that is not the main core tries to create
// a sub agent.
var ErrNotMain = errors.New("subagent: only the main core may create a sub agent")

// ErrNoSuchAgent is returned when a message addresses an agent nobody registered.
var ErrNoSuchAgent = errors.New("subagent: no such agent")

// Journal is the broker's durable half.
//
// It is an interface because the point of the design is that the broker writes
// before it delivers, and a test needs to be able to make that write fail on
// demand. A real caller passes the storage write buffer, which batches; a test
// passes a stub that records the order of calls.
type Journal interface {
	// WriteSync writes immediately and returns only once the bytes have been
	// handed to the operating system. Batching is not available here on purpose.
	WriteSync(ctx context.Context, records []storage.Record) error
	// LastSeq reports the last sequence written for a stream.
	LastSeq(ctx context.Context, stream string) (int64, error)
}

// Broker carries messages between agents.
//
// The delivery order is fixed and is the whole point: a point to point message is
// written to the journal before it reaches a mailbox, so a process that dies
// between the two has left a record of work that was promised. The cost is an
// inline write per point to point message, which is affordable because there are
// orders of magnitude fewer of them than events.
type Broker struct {
	journal Journal
	stream  string

	mu        sync.RWMutex
	mailboxes map[string]*Mailbox
	// mainID is the main core's identifier, so a message can address it by role.
	mainID string
	// sequence is the per stream position, assigned under the same lock as the
	// mailbox lookup so a message's sequence and its delivery agree.
	sequence int64
	// counter is a message identifier source. It is a counter rather than a random
	// value so a test can name a message it expects to see.
	counter uint64
	// sent and delivered are the two numbers that differ, and the gap is what the
	// journal-first guarantee is about.
	sent      int64
	delivered int64
	orphaned  int64
	closed    bool
}

// BrokerConfig configures a broker.
type BrokerConfig struct {
	// Journal receives the messages. A nil journal makes every send fail, which is
	// the correct behaviour: a broker that cannot promise durability should not
	// pretend to deliver.
	Journal Journal
	// Stream is the journal stream name, normally the one the storage scope uses
	// for the broker.
	Stream string
	// MainID is the main core's identifier.
	MainID string
}

// NewBroker builds a broker.
func NewBroker(config BrokerConfig) (*Broker, error) {
	if config.Journal == nil {
		return nil, fmt.Errorf("subagent: a broker needs a journal")
	}
	if config.Stream == "" {
		config.Stream = "broker:journal"
	}
	if config.MainID == "" {
		return nil, fmt.Errorf("subagent: a broker needs the main core identifier")
	}
	broker := &Broker{
		journal:   config.Journal,
		stream:    config.Stream,
		mainID:    config.MainID,
		mailboxes: map[string]*Mailbox{},
	}
	// A message addressed to the main core by role reaches the same mailbox as one
	// addressed by identifier, registered at startup so the route is never missing.
	broker.mailboxes[config.MainID] = newMailbox(config.MainID)
	return broker, nil
}

// Register gives an agent a mailbox. Registering an identifier twice keeps the
// existing mailbox, because replacing it would silently drop whatever the agent
// had not read yet.
func (b *Broker) Register(agentID string) error {
	// An identifier that is only whitespace is an identifier nobody can address, so
	// it is rejected rather than registered under a name that will not match.
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("subagent: an agent needs an identifier")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("subagent: the broker is closed")
	}
	if _, exists := b.mailboxes[agentID]; exists {
		return nil
	}
	b.mailboxes[agentID] = newMailbox(agentID)
	return nil
}

// Unregister removes an agent and closes its mailbox.
//
// The messages still in it are not delivered to anyone: the agent is gone, which
// is the same situation a crashed agent leaves behind, and filing them for audit
// is more honest than handing them to the next agent that registers.
func (b *Broker) Unregister(agentID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	mailbox, exists := b.mailboxes[agentID]
	if !exists {
		return
	}
	delete(b.mailboxes, agentID)
	mailbox.close()
}

// Mailbox returns an agent's mailbox.
func (b *Broker) Mailbox(agentID string) (*Mailbox, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	mailbox, exists := b.mailboxes[agentID]
	return mailbox, exists
}

// Agents reports the registered identifiers.
func (b *Broker) Agents() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	agents := make([]string, 0, len(b.mailboxes))
	for agentID := range b.mailboxes {
		agents = append(agents, agentID)
	}
	return agents
}

// Send delivers a point to point message.
//
// The write happens first and synchronously. If it fails, nothing is delivered:
// a message that was never promised must not arrive, and one that was promised
// must not silently vanish.
func (b *Broker) Send(ctx context.Context, message Message) (Message, error) {
	if err := message.validate(); err != nil {
		return Message{}, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return Message{}, fmt.Errorf("subagent: the broker is closed")
	}
	if message.ID == "" {
		b.counter++
		message.ID = fmt.Sprintf("msg-%d", b.counter)
	}
	if message.Timestamp.IsZero() {
		message.Timestamp = timeNow()
	}
	// The journal runs first. Everything after this point may be lost to a crash
	// and that is acceptable, because the record of it is already on disk.
	encoded, err := encode(message)
	if err != nil {
		b.mu.Unlock()
		return Message{}, err
	}
	record := storage.Record{
		Stream: b.stream,
		Type:   storage.RecordBrokerMessage,
		Data:   encoded,
		Ts:     message.Timestamp,
	}
	writeErr := b.journal.WriteSync(ctx, []storage.Record{record})
	if writeErr != nil {
		b.mu.Unlock()
		return Message{}, fmt.Errorf("subagent: the message was not delivered because the journal refused it: %w", writeErr)
	}
	sequence, seqErr := b.journal.LastSeq(ctx, b.stream)
	if seqErr == nil {
		message.Sequence = sequence
	}
	b.sent++
	b.mu.Unlock()

	delivered := b.route(message)
	if delivered {
		b.mu.Lock()
		b.delivered++
		b.mu.Unlock()
	}
	return message, nil
}

// route hands a message to its destinations and reports whether every destination
// took it.
func (b *Broker) route(message Message) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	switch message.ToAgentID {
	case RouteBroadcast:
		accepted := true
		for _, mailbox := range b.mailboxes {
			if !mailbox.deliver(message) {
				accepted = false
			}
		}
		return accepted
	case RouteMain:
		mailbox, exists := b.mailboxes[b.mainID]
		if !exists {
			return false
		}
		return mailbox.deliver(message)
	default:
		mailbox, exists := b.mailboxes[message.ToAgentID]
		if !exists {
			return false
		}
		return mailbox.deliver(message)
	}
}

// Broadcast sends a message to every registered agent, including the main core.
func (b *Broker) Broadcast(ctx context.Context, message Message) (Message, error) {
	message.ToAgentID = RouteBroadcast
	return b.Send(ctx, message)
}

// ToMain sends a message to the main core without the caller knowing its
// identifier.
func (b *Broker) ToMain(ctx context.Context, message Message) (Message, error) {
	message.ToAgentID = RouteMain
	return b.Send(ctx, message)
}

// FileOrphans records messages found in the journal at startup without delivering
// them.
//
// A restarted process has no sub agents, so a message in the journal belongs to an
// agent that no longer exists. Delivering it would restart work nobody asked for,
// so every one of them is filed as an orphan and returned for audit replay.
func (b *Broker) FileOrphans(ctx context.Context, messages []Message) ([]Message, error) {
	if len(messages) == 0 {
		return nil, nil
	}
	orphans := make([]Message, 0, len(messages))
	records := make([]storage.Record, 0, len(messages))
	for _, message := range messages {
		orphan := message.Clone()
		orphan.Orphan = true
		encoded, err := encode(orphan)
		if err != nil {
			return nil, err
		}
		orphans = append(orphans, orphan)
		records = append(records, storage.Record{
			Stream: b.stream,
			Type:   storage.RecordBrokerMessage,
			Data:   encoded,
			Ts:     message.Timestamp,
		})
	}
	if err := b.journal.WriteSync(ctx, records); err != nil {
		return nil, fmt.Errorf("subagent: filing orphan messages: %w", err)
	}
	b.mu.Lock()
	b.orphaned += int64(len(orphans))
	b.mu.Unlock()
	return orphans, nil
}

// Stats reports what the broker has done. Sent and delivered are separate numbers
// because the gap between them is the whole durability story.
type Stats struct {
	// Sent counts messages whose journal write succeeded.
	Sent int64 `json:"sent"`
	// Delivered counts messages a mailbox took.
	Delivered int64 `json:"delivered"`
	// Orphaned counts messages filed without delivery.
	Orphaned int64 `json:"orphaned"`
	// Agents is how many mailboxes exist.
	Agents int `json:"agents"`
	// Dropped sums the messages mailboxes could not hold.
	Dropped int64 `json:"dropped"`
}

// Stats reports the broker's counters.
func (b *Broker) Stats() Stats {
	b.mu.RLock()
	defer b.mu.RUnlock()
	stats := Stats{
		Sent:      b.sent,
		Delivered: b.delivered,
		Orphaned:  b.orphaned,
		Agents:    len(b.mailboxes),
	}
	for _, mailbox := range b.mailboxes {
		stats.Dropped += mailbox.Dropped()
	}
	return stats
}

// Close shuts the broker and every mailbox down.
func (b *Broker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	mailboxes := make([]*Mailbox, 0, len(b.mailboxes))
	for _, mailbox := range b.mailboxes {
		mailboxes = append(mailboxes, mailbox)
	}
	b.mailboxes = map[string]*Mailbox{}
	b.mu.Unlock()
	for _, mailbox := range mailboxes {
		mailbox.close()
	}
}
