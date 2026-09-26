// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// The design promises a point to point broker message reaches the journal in under
// a hundred microseconds, because the inline write is what makes journal first
// delivery possible at all. These gates measure it.

// percentile returns the value at a given percentile of a sorted sample.
func percentile(samples []time.Duration, fraction float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(first, second int) bool { return samples[first] < samples[second] })
	index := int(float64(len(samples)) * fraction)
	if index >= len(samples) {
		index = len(samples) - 1
	}
	return samples[index]
}

// nullJournal stands in for the write buffer, so the measurement is of the
// broker's own path rather than of a disk that is not there.
type nullJournal struct {
	mu        sync.Mutex
	sequence  int64
	delivered int64
}

func (j *nullJournal) WriteSync(_ context.Context, records []storage.Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.sequence += int64(len(records))
	j.delivered++
	return nil
}

func (j *nullJournal) LastSeq(context.Context, string) (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.sequence, nil
}

func TestBrokerPerformanceGates(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to run the broker performance gates")
	}
	journal := &nullJournal{}
	broker, err := NewBroker(BrokerConfig{Journal: journal, MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	if err := broker.Register("child"); err != nil {
		t.Fatal(err)
	}
	// The mailbox has to be drained, or the sends stop being measured and start
	// being reported as dropped.
	stop := make(chan struct{})
	var drained sync.WaitGroup
	drained.Add(1)
	go func() {
		defer drained.Done()
		mailbox, _ := broker.Mailbox("child")
		for {
			select {
			case <-stop:
				return
			default:
			}
			mailbox.ReceiveWithin(time.Millisecond)
		}
	}()

	const sends = 2000
	samples := make([]time.Duration, 0, sends)
	for index := 0; index < sends; index++ {
		started := time.Now()
		if _, err := broker.Send(context.Background(), Message{
			FromAgentID: "main", ToAgentID: "child", Type: TypeTask,
			Content: "do the work " + strings.Repeat("x", index%64),
		}); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, time.Since(started))
	}
	close(stop)
	drained.Wait()

	p99 := percentile(samples, 0.99)
	// The clock on this host is coarse enough to report zero for a sub
	// microsecond send, so the gate is written against the median as well: a
	// number that always reads zero would otherwise pass anything.
	median := percentile(samples, 0.5)
	t.Logf("point to point send median = %v, p99 = %v over %d sends", median, p99, sends)
	if p99 > 100*time.Microsecond {
		t.Fatalf("point to point send p99 = %v, the design promises under 100µs", p99)
	}
	if median > 50*time.Microsecond {
		t.Fatalf("point to point send median = %v, which is not a journal first send", median)
	}
	// Every send reached the journal, which is the guarantee the measurement is
	// really about.
	stats := broker.Stats()
	if stats.Sent != sends {
		t.Fatalf("sent = %d", stats.Sent)
	}
	if journal.delivered != sends {
		t.Fatalf("the journal saw %d writes", journal.delivered)
	}
}

func BenchmarkSendPointToPoint(b *testing.B) {
	journal := &nullJournal{}
	broker, err := NewBroker(BrokerConfig{Journal: journal, MainID: "main"})
	if err != nil {
		b.Fatal(err)
	}
	defer broker.Close()
	if err := broker.Register("child"); err != nil {
		b.Fatal(err)
	}
	go func() {
		mailbox, _ := broker.Mailbox("child")
		for {
			if _, ok := mailbox.ReceiveWithin(time.Millisecond); !ok {
				return
			}
		}
	}()
	message := Message{
		FromAgentID: "main", ToAgentID: "child", Type: TypeTask, Content: "do the work",
	}
	ctx := context.Background()
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := broker.Send(ctx, message); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSendBroadcast(b *testing.B) {
	journal := &nullJournal{}
	broker, err := NewBroker(BrokerConfig{Journal: journal, MainID: "main"})
	if err != nil {
		b.Fatal(err)
	}
	defer broker.Close()
	for index := 0; index < 8; index++ {
		if err := broker.Register("child-" + strings.Repeat("x", index)); err != nil {
			b.Fatal(err)
		}
	}
	go func() {
		for {
			busy := false
			for _, agentID := range broker.Agents() {
				if mailbox, ok := broker.Mailbox(agentID); ok {
					mailbox.ReceiveWithin(time.Millisecond)
					busy = true
				}
			}
			if !busy {
				return
			}
		}
	}()
	ctx := context.Background()
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := broker.Broadcast(ctx, Message{
			FromAgentID: "main", Type: TypeStatus, Content: "status",
		}); err != nil {
			b.Fatal(err)
		}
	}
}
