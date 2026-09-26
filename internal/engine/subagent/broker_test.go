// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// recordingJournal is a journal that records the order of its calls, which is the
// only way to prove that a write happened before a delivery.
type recordingJournal struct {
	mu sync.Mutex
	// events records what happened, in order, as "write" and "deliver".
	events []string
	// writes counts the records written.
	writes int
	// sequences is the per stream counter the journal hands back.
	sequences int64
	// failWrite makes the write fail, so the delivery guarantee can be tested when
	// the disk says no.
	failWrite error
	// failLastSeq makes the sequence lookup fail, which must not stop delivery.
	failLastSeq bool
	// records keeps what was written, for the orphan test.
	records []storage.Record
}

func (j *recordingJournal) WriteSync(_ context.Context, records []storage.Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failWrite != nil {
		j.events = append(j.events, "write-failed")
		return j.failWrite
	}
	j.events = append(j.events, "write")
	j.records = append(j.records, records...)
	j.writes += len(records)
	j.sequences += int64(len(records))
	return nil
}

func (j *recordingJournal) LastSeq(_ context.Context, _ string) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failLastSeq {
		return 0, errors.New("the sequence lookup failed")
	}
	return j.sequences, nil
}

func (j *recordingJournal) note(event string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event)
}

func (j *recordingJournal) seen() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(j.events))
	copy(out, j.events)
	return out
}

func TestSendWritesTheJournalBeforeDelivering(t *testing.T) {
	journal := &recordingJournal{}
	broker := newTestBroker(t, journal)
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, exists := broker.Mailbox("child")
	if !exists {
		t.Fatal("the mailbox was not registered")
	}
	// The order has to be observed, not inferred. The journal notes its own write
	// and the receiver notes its own delivery into one ordered list, so the
	// sequence in that list is the guarantee itself.
	received := make(chan Message, 1)
	go func() {
		message, ok := mailbox.ReceiveWithin(5 * time.Second)
		if !ok {
			close(received)
			return
		}
		journal.note("deliver")
		received <- message
	}()

	sent, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: "child", Type: TypeTask, Content: "do the work",
	})
	if err != nil {
		t.Fatal(err)
	}
	message, ok := <-received
	if !ok {
		t.Fatal("the message was never delivered")
	}
	if message.Content != "do the work" {
		t.Fatalf("received = %#v", message)
	}
	seen := journal.seen()
	if len(seen) != 2 || seen[0] != "write" || seen[1] != "deliver" {
		t.Fatalf("journal events = %v, want the write before the delivery", seen)
	}
	// The sequence came back from the journal, so the audit trail and the message
	// agree on where it sits in the stream.
	if sent.Sequence == 0 {
		t.Fatal("the message came back without a sequence")
	}
	if sent.ID == "" {
		t.Fatal("the message came back without an identifier")
	}
}

func newTestBroker(t *testing.T, journal Journal) *Broker {
	t.Helper()
	broker, err := NewBroker(BrokerConfig{Journal: journal, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	return broker
}

func TestNewBrokerRequiresItsCollaborators(t *testing.T) {
	if _, err := NewBroker(BrokerConfig{MainID: "main"}); err == nil {
		t.Fatal("a broker without a journal was accepted")
	}
	if _, err := NewBroker(BrokerConfig{Journal: &recordingJournal{}}); err == nil {
		t.Fatal("a broker without a main core identifier was accepted")
	}
	broker, err := NewBroker(BrokerConfig{Journal: &recordingJournal{}, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// A stream name defaults rather than being required, because the storage scope
	// has one canonical name for it.
	broker.Close()
}

func TestSendRefusesToDeliverWhenTheJournalRefuses(t *testing.T) {
	// This is the whole guarantee. A message that was never written must not
	// arrive, because a caller reading it would act on work that no audit trail
	// knows about.
	sentinel := errors.New("the disk said no")
	journal := &recordingJournal{failWrite: sentinel}
	broker := newTestBroker(t, journal)
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := broker.Mailbox("child")
	_, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: "child", Type: TypeTask, Content: "do the work",
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if mailbox.Len() != 0 {
		t.Fatal("a message was delivered after the journal refused it")
	}
	stats := broker.Stats()
	if stats.Sent != 0 || stats.Delivered != 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestSendValidatesItsMessage(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	cases := []struct {
		name    string
		message Message
	}{
		{"no sender", Message{ToAgentID: "child", Type: TypeTask, Timestamp: timeNow()}},
		{"no recipient", Message{FromAgentID: "main", Type: TypeTask, Timestamp: timeNow()}},
		{"no type", Message{FromAgentID: "main", ToAgentID: "child", Timestamp: timeNow()}},
		{"blank sender", Message{FromAgentID: "  ", ToAgentID: "child", Type: TypeTask}},
		{"blank recipient", Message{FromAgentID: "main", ToAgentID: " ", Type: TypeTask}},
		{"blank type", Message{FromAgentID: "main", ToAgentID: "child", Type: "\t"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := broker.Send(context.Background(), testCase.message); err == nil {
				t.Fatal("an incomplete message was accepted")
			}
		})
	}
}

func TestSendFillsInWhatTheCallerLeftOut(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	// An identifier and a timestamp are the broker's to assign, because a caller
	// that invented them could collide with another caller's.
	sent, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: "child", Type: TypeTask,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.ID == "" || sent.Timestamp.IsZero() {
		t.Fatalf("sent = %#v", sent)
	}
}

func TestRoutesReachTheRightMailboxes(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	for _, agentID := range []string{"one", "two"} {
		if err := broker.Register(agentID); err != nil {
			t.Fatal(err)
		}
	}
	// A broadcast reaches every agent, the main core included, because the main
	// core is an agent like any other and may want to hear it.
	if _, err := broker.Broadcast(context.Background(), Message{
		FromAgentID: "main", Type: TypeStatus, Content: "status",
	}); err != nil {
		t.Fatal(err)
	}
	for _, agentID := range broker.Agents() {
		mailbox, _ := broker.Mailbox(agentID)
		message, ok := mailbox.ReceiveWithin(time.Second)
		if !ok {
			t.Fatalf("%s did not receive the broadcast", agentID)
		}
		if message.Content != "status" {
			t.Fatalf("%s received %#v", agentID, message)
		}
	}
	// The main core is addressable by role, so a caller does not have to know the
	// identifier it was assigned.
	sent, err := broker.ToMain(context.Background(), Message{
		FromAgentID: "one", Type: TypeResult, Content: "done",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.ToAgentID != RouteMain {
		t.Fatalf("to = %q", sent.ToAgentID)
	}
	main, _ := broker.Mailbox("main")
	if _, ok := main.ReceiveWithin(time.Second); !ok {
		t.Fatal("the main core received nothing")
	}
}

func TestSendToAnUnknownAgentIsWrittenButNotDelivered(t *testing.T) {
	journal := &recordingJournal{}
	broker := newTestBroker(t, journal)
	// The journal still records it: a message addressed to an agent that vanished
	// is exactly what an auditor needs to see.
	sent, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: "ghost", Type: TypeQuery, Timestamp: timeNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Sequence == 0 {
		t.Fatal("the message was not written")
	}
	stats := broker.Stats()
	if stats.Sent != 1 || stats.Delivered != 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestMailboxReportsWhatItCouldNotHold(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := broker.Mailbox("child")
	// A full mailbox is reported rather than blocking. A sender blocked on a stuck
	// receiver would turn one slow agent into a stalled session, and the journal
	// already has the message.
	for index := 0; index < MailboxCapacity; index++ {
		if err := mailbox.deliver(Message{ID: fmt.Sprint(index)}); !err {
			t.Fatalf("message %d was refused with room to spare", index)
		}
	}
	if err := mailbox.deliver(Message{ID: "overflow"}); err {
		t.Fatal("a full mailbox accepted a message")
	}
	if mailbox.Dropped() != 1 {
		t.Fatalf("dropped = %d", mailbox.Dropped())
	}
	if mailbox.Len() != MailboxCapacity {
		t.Fatalf("len = %d", mailbox.Len())
	}
	// A closed mailbox refuses new messages but still hands over what it already
	// held. Draining is Go's channel behaviour and it is the right one: an agent
	// that stopped should still be able to read the task it was given before it
	// stopped, and then it runs dry.
	draining := newMailbox("child")
	if err := draining.deliver(Message{ID: "held"}); !err {
		t.Fatal(err)
	}
	draining.close()
	if draining.deliver(Message{ID: "late"}) {
		t.Fatal("a closed mailbox accepted a message")
	}
	if _, ok := draining.ReceiveWithin(0); !ok {
		t.Fatal("a closed mailbox dropped the message it already held")
	}
	if _, ok := draining.ReceiveWithin(0); ok {
		t.Fatal("a closed mailbox kept delivering after it ran dry")
	}
	draining.close()
	mailbox.close()
}

func TestMailboxReceiveWithinGivesUp(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := broker.Mailbox("child")
	if _, ok := mailbox.ReceiveWithin(0); ok {
		t.Fatal("an empty mailbox produced a message")
	}
	if _, ok := mailbox.ReceiveWithin(-time.Second); ok {
		t.Fatal("an empty mailbox produced a message")
	}
	if err := mailbox.deliver(Message{ID: "one", Content: "here"}); !err {
		t.Fatal(err)
	}
	message, ok := mailbox.ReceiveWithin(time.Second)
	if !ok || message.Content != "here" {
		t.Fatalf("message = %#v ok = %v", message, ok)
	}
}

func TestMailboxHandsOutCopies(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := broker.Mailbox("child")
	original := Message{ID: "one", Data: []byte("payload")}
	mailbox.mailbox <- original
	received, _ := mailbox.Receive()
	received.Data[0] = 'X'
	// The sender's buffer must not be reachable through what the receiver got.
	if string(original.Data) != "payload" {
		t.Fatal("the receiver reached into the sender's message")
	}
}

func TestUnregisterClosesTheMailboxWithoutLosingTheRecord(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := broker.Mailbox("child")
	broker.Unregister("child")
	if _, exists := broker.Mailbox("child"); exists {
		t.Fatal("the mailbox survived unregistration")
	}
	if _, ok := mailbox.Receive(); ok {
		t.Fatal("an unregistered mailbox still delivered")
	}
	// Unregistering something that is not there is a no op, so shutdown paths have
	// no ordering requirement.
	broker.Unregister("never-registered")
}

func TestFileOrphansRecordsWithoutDelivering(t *testing.T) {
	journal := &recordingJournal{}
	broker := newTestBroker(t, journal)
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := broker.Mailbox("child")
	recovered := []Message{
		{ID: "m1", FromAgentID: "main", ToAgentID: "child", Type: TypeTask,
			Content: "unclaimed work", Timestamp: timeNow()},
		{ID: "m2", FromAgentID: "main", ToAgentID: RouteBroadcast, Type: TypeTask,
			Content: "unclaimed broadcast", Timestamp: timeNow()},
	}
	orphans, err := broker.FileOrphans(context.Background(), recovered)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 2 {
		t.Fatalf("orphans = %d", len(orphans))
	}
	for _, orphan := range orphans {
		if !orphan.Orphan {
			t.Fatalf("orphan = %#v", orphan)
		}
	}
	// Nothing was delivered. A restarted process has no sub agent to receive it,
	// and redelivering would restart work nobody asked for.
	if mailbox.Len() != 0 {
		t.Fatal("an orphan was delivered")
	}
	stats := broker.Stats()
	if stats.Orphaned != 2 || stats.Delivered != 0 {
		t.Fatalf("stats = %#v", stats)
	}
	// Filing an empty set does nothing at all, not even a write.
	before := stats.Orphaned
	if _, err := broker.FileOrphans(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if broker.Stats().Orphaned != before {
		t.Fatal("an empty orphan set was filed")
	}
}

func TestFileOrphansReportsAJournalFailure(t *testing.T) {
	sentinel := errors.New("the disk said no")
	broker := newTestBroker(t, &recordingJournal{failWrite: sentinel})
	if _, err := broker.FileOrphans(context.Background(), []Message{{
		ID: "m1", FromAgentID: "main", ToAgentID: "child", Type: TypeTask, Timestamp: timeNow(),
	}}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
}

func TestSequenceLookupFailureDoesNotStopDelivery(t *testing.T) {
	// The sequence is an audit convenience. Losing it must not lose the message,
	// because the record is already on disk either way.
	journal := &recordingJournal{failLastSeq: true}
	broker := newTestBroker(t, journal)
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := broker.Mailbox("child")
	sent, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: "child", Type: TypeTask, Timestamp: timeNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Sequence != 0 {
		t.Fatalf("sequence = %d", sent.Sequence)
	}
	if _, ok := mailbox.ReceiveWithin(time.Second); !ok {
		t.Fatal("the message was not delivered")
	}
}

func TestClosedBrokerRefusesEverything(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	broker.Close()
	if _, err := broker.Send(context.Background(), Message{
		FromAgentID: "main", ToAgentID: "child", Type: TypeTask, Timestamp: timeNow(),
	}); err == nil {
		t.Fatal("a closed broker accepted a send")
	}
	if err := broker.Register("late"); err == nil {
		t.Fatal("a closed broker accepted a registration")
	}
	broker.Close()
	if len(broker.Agents()) != 0 {
		t.Fatal("mailboxes survived the close")
	}
}

func TestRegisterRejectsAnEmptyIdentifier(t *testing.T) {
	broker := newTestBroker(t, &recordingJournal{})
	if err := broker.Register("  "); err == nil {
		t.Fatal("an empty identifier was accepted")
	}
	// Registering twice keeps the existing mailbox, because replacing it would
	// silently drop whatever the agent had not read yet.
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := broker.Mailbox("child")
	mailbox.mailbox <- Message{ID: "pending"}
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	again, _ := broker.Mailbox("child")
	if again.Len() != 1 {
		t.Fatal("re-registering replaced the mailbox and lost a message")
	}
}

func TestMessagesSurviveTheJournalRoundTrip(t *testing.T) {
	original := Message{
		ID:          "m1",
		FromAgentID: "main",
		ToAgentID:   "child",
		Type:        TypeResult,
		Content:     "the result",
		Data:        []byte(`{"count":3}`),
		Timestamp:   time.Unix(1700000000, 0).UTC(),
	}
	encoded, err := encode(original)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ID != original.ID || decoded.Content != original.Content {
		t.Fatalf("decoded = %#v", decoded)
	}
	if string(decoded.Data) != `{"count":3}` {
		t.Fatalf("data = %s", decoded.Data)
	}
	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Fatalf("timestamp = %v", decoded.Timestamp)
	}
	if _, err := decode([]byte("{")); err == nil {
		t.Fatal("a malformed record was decoded")
	}
	// Every field of a message is a type that always marshals, so arbitrary bytes
	// in the payload survive rather than failing the send.
	arbitrary := Message{
		ID: "m2", FromAgentID: "main", ToAgentID: "child", Type: TypeStatus,
		Data: []byte{0xff, 0xfe, 0x00}, Timestamp: time.Unix(1, 0).UTC(),
	}
	encoded, err = encode(arbitrary)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Data) != len(arbitrary.Data) {
		t.Fatalf("data = %v", decoded.Data)
	}
}
