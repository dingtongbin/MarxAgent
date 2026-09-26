// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// A result the pool could not file is a task that did not come right, and the main
// core has to hear about it. A pool that quietly swallowed the failure would leave the
// registry saying one thing and the slot saying another, with nothing to say which.
func TestAResultThatCannotBeFiledIsReported(t *testing.T) {
	registry, err := NewRegistry(RegistryConfig{MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewBroker(BrokerConfig{MainID: "main", Journal: &recordingJournal{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	pool, err := NewPool(PooledAgentConfig{
		MainID: "main", Registry: registry, Broker: broker,
		Templates: templatesFor(t), Builder: &recordingBuilder{}, Capacity: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	// The slot is created and registered together, so the only way to get a task that
	// cannot be filed is for the registry to lose it in between. That is a real fault
	// and it must not look like a task that worked.
	if err := registry.Delete(context.Background(), "main", slot.ID); err != nil {
		t.Fatal(err)
	}
	result, err := runTaskOn(pool, slot.ID, "look")
	if err == nil {
		t.Fatal("a task whose result could not be filed reported success")
	}
	if result.State != StateFailed {
		t.Fatalf("the result is %q", result.State)
	}
	if !strings.Contains(result.Err, slot.ID) {
		t.Fatalf("the failure does not say which slot: %q", result.Err)
	}
	// And the slot still comes back, because a pool that could not file a result is
	// still a pool.
	if after, _ := pool.GetSlot(slot.ID); after.State == SlotBusy {
		t.Fatal("the slot stayed busy")
	}
}

// A pool with no broker is a legitimate thing to build for work that only reports
// back, so it must run rather than fall over when the result is filed.
func TestAPoolWithNoBrokerStillRuns(t *testing.T) {
	registry, err := NewRegistry(RegistryConfig{MainID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(PooledAgentConfig{
		MainID: "main", Registry: registry,
		Templates: templatesFor(t), Builder: &recordingBuilder{}, Capacity: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	// Work offered before any slot exists is refused rather than waited on, because a
	// slot only appears when the main core asks for one and it has not.
	if _, err := pool.Start(context.Background(), "main", Task{
		Input: core.Input{Type: core.InputTypeText, Content: "look"},
	}); !errors.Is(err, ErrNoSlots) {
		t.Fatalf("work offered to an empty pool: %v", err)
	}
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	result, err := runTaskOn(pool, slot.ID, "look")
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateDone {
		t.Fatalf("the result is %q", result.State)
	}
	// With no broker there are no messages, and reporting a number for them would be
	// inventing one.
	if result.Messages != 0 {
		t.Fatalf("the result claims %d messages", result.Messages)
	}
}

// An error event that does not say what went wrong is still an error. Passing it on
// as a task that worked because the text was missing would be the worst possible
// reading of it.
func TestAnErrorWithoutAReasonIsStillAFailure(t *testing.T) {
	pool, _, _ := newTestPool(t, &scriptedBuilder{events: []core.Event{
		{Type: "partial", Data: "working"},
		{Type: "error"},
	}}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	result, err := runTaskOn(pool, slot.ID, "look")
	if err == nil {
		t.Fatal("an error event was filed as a task that worked")
	}
	if result.State != StateFailed {
		t.Fatalf("the result is %q", result.State)
	}
	if !strings.Contains(result.Err, "without saying why") {
		t.Fatalf("the failure does not admit it knows nothing: %q", result.Err)
	}
}

// An error that says what happened is carried through, because the main core is the
// one who has to decide what to do about it.
func TestTheReasonForAFailureReachesTheMainCore(t *testing.T) {
	pool, _, _ := newTestPool(t, &scriptedBuilder{events: []core.Event{
		{Type: "error", Error: "the model endpoint refused the request"},
	}}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	result, err := runTaskOn(pool, slot.ID, "look")
	if err == nil {
		t.Fatal("a failed task reported success")
	}
	if !strings.Contains(result.Err, "refused the request") {
		t.Fatalf("the reason was lost: %q", result.Err)
	}
	after, _ := pool.GetSlot(slot.ID)
	if !strings.Contains(after.LastErr, "refused the request") {
		t.Fatalf("the slot did not keep the reason: %q", after.LastErr)
	}
}

// A caller that stops waiting has to be let go, and the task it was waiting for keeps
// running, because the sub agent is not the caller's to abandon.
func TestAWaiterCanGiveUpWithoutStoppingTheSubAgent(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{delay: 300 * time.Millisecond}, 1, 1)
	slot, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	busy, err := pool.Start(context.Background(), "main", Task{
		Slot:  slot.ID,
		Input: core.Input{Type: core.InputTypeText, Content: "slow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := pool.Wait(ctx, busy.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting gave %v", err)
	}
	// The sub agent carried on and finished, so a caller that came back later still
	// finds the answer.
	result, err := pool.Wait(context.Background(), busy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateDone {
		t.Fatalf("the task was disturbed by the waiter leaving: %q", result.State)
	}
}

// A slot the registry will not take must not be left behind. A slot that exists in the
// pool and not in the registry is a sub agent the main core cannot see, which is worse
// than a slot that was never created.
func TestASlotTheRegistryRefusesIsNotLeftBehind(t *testing.T) {
	// The registry holds the main core too, so one more agent is one slot.
	registry, err := NewRegistry(RegistryConfig{MainID: "main", MaxAgents: 2})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(PooledAgentConfig{
		MainID: "main", Registry: registry,
		Templates: templatesFor(t), Builder: &recordingBuilder{}, Capacity: 4, MaxConcurrent: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	first, err := pool.CreateSlot(context.Background(), "main", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.CreateSlot(context.Background(), "main", "reviewer"); err == nil {
		t.Fatal("the pool took a slot the registry refused")
	}
	if got := pool.Stats(); got.Slots != 1 {
		t.Fatalf("the pool holds %d slots after a refused one", got.Slots)
	}
	if got := pool.Stats(); got.Capacity != 4 || got.Available != 4 {
		t.Fatalf("the refused slot is still counted: %d of %d free", got.Available, got.Capacity)
	}
	// And the slot that was taken is untouched.
	if _, ok := pool.GetSlot(first.ID); !ok {
		t.Fatal("the first slot went away")
	}
}

// A pool reports a name that is not there rather than handing back whichever slot
// happens to be free.
func TestCreatingWithATemplateThatDoesNotExist(t *testing.T) {
	pool, _, _ := newTestPool(t, &recordingBuilder{}, 1, 1)
	if _, err := pool.CreateSlot(context.Background(), "main", "no-such-template"); err == nil {
		t.Fatal("a slot was created for a template that does not exist")
	}
	if got := pool.Stats(); got.Slots != 0 {
		t.Fatalf("the pool holds %d slots", got.Slots)
	}
}
