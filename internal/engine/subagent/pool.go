// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

var (
	// ErrPoolFull is a request for a slot when every slot is busy. It is a refusal
	// rather than a queue, because a caller that has to be told no can decide what to
	// give up, and a caller that waits cannot.
	ErrPoolFull = errors.New("subagent: every slot is busy")
	// ErrNoSlot is a reference to a slot that was never created or has been deleted.
	ErrNoSlot = errors.New("subagent: no such slot")
	// ErrSlotStopped is work offered to a slot the main core has stopped. It is
	// refused rather than queued, because a stopped slot is not coming back on its own
	// and a caller waiting for one would wait for good.
	ErrSlotStopped = errors.New("subagent: the slot was stopped")
	// ErrNoSlots is work offered to a pool that has no slots. Slots are made by the
	// main core asking for them, so an empty pool stays empty until it does.
	ErrNoSlots = errors.New("subagent: the pool has no slots")
)

// Task is one unit of work for a slot.
type Task struct {
	// Slot names the slot to run on. Empty asks for any free slot, which is the
	// ordinary case: a caller that cares which slot runs has already decided more
	// than it needs to.
	Slot string
	// Input is what the sub agent is asked to do.
	Input core.Input
	// Summary is what the main core will be told the sub agent did. It is given here
	// rather than derived afterwards because a summary written by whoever asked is
	// the one that answers the question they are asking.
	Summary string
}

// TaskResult is what a finished task produced.
type TaskResult struct {
	// Slot is the slot that ran it.
	Slot string `json:"slot"`
	// AgentID is the sub agent's identifier.
	AgentID string `json:"agent_id"`
	// Summary is what the caller said it was for.
	Summary string `json:"summary,omitempty"`
	// State is where the sub agent ended up.
	State State `json:"state"`
	// Messages is how many messages the sub agent exchanged, so a caller can tell a
	// busy sub agent from an idle one without reading its transcript.
	Messages int64 `json:"messages"`
	// Duration is how long it took.
	Duration time.Duration `json:"duration"`
	// Err is why it failed, empty otherwise.
	Err string `json:"error,omitempty"`
}

// SlotState is where a slot is in its life. A slot outlives the task on it, so this
// is not the state of a sub agent: it is the state of a place one is run.
type SlotState string

const (
	// SlotIdle is a slot with nothing on it and available.
	SlotIdle SlotState = "idle"
	// SlotBusy is a slot running a task.
	SlotBusy SlotState = "busy"
	// SlotStopped is a slot that has been stopped and will not run again until it is
	// started.
	SlotStopped SlotState = "stopped"
	// SlotFailed is a slot whose last task failed. It is idle again and this says why,
	// so a caller can see a failure after the fact rather than only while it happened.
	SlotFailed SlotState = "failed"
)

// SlotRecord is what a caller sees of a slot.
//
// It is a separate type rather than the slot itself because a slot owns a lock, and
// handing a lock out by value copies it. A caller that holds a copy of a lock is a
// caller whose view of the slot stops being the slot's.
type SlotRecord struct {
	// ID is the slot's identifier, which is also the sub agent's while it is running.
	ID string `json:"id"`
	// Template is the template it runs.
	Template string `json:"template"`
	// State is where it is in its life.
	State SlotState `json:"state"`
	// Runs is how many tasks it has run, which is what makes a pool worth having
	// visible: reuse is the point, and a number that never moves would be a pool
	// that is not one.
	Runs int64 `json:"runs"`
	// CreatedAt and UpdatedAt bound its life.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// LastSummary is what the last task was for.
	LastSummary string `json:"last_summary,omitempty"`
	// LastErr is why the last task failed.
	LastErr string `json:"error,omitempty"`
	// LastDuration is how long the last task took.
	LastDuration time.Duration `json:"last_duration"`
}

// Slot is one place a sub agent runs.
//
// The slot is the reusable part: it owns the identifier, the mailbox, the registry
// entry and the claim on a concurrency slot. The agent on it is not reusable, and
// that is the whole reason the two are separate. A core agent holds its
// conversation, there is no way to clear one, and a sub agent that inherited the
// previous task's transcript would be answering a question nobody asked it.
type Slot struct {
	SlotRecord

	pool *Pool

	mu     sync.Mutex
	cancel context.CancelFunc
	// agent is the core agent currently on this slot, nil when idle.
	agent core.Agent
	// events carries the running task's events, so the main core can watch a sub
	// agent work rather than only its result.
	events chan core.Event
	// finished is closed when the task on this slot ends, so a caller that needs the
	// answer can wait for it.
	finished chan struct{}
	// result is what the last task produced, kept so the main core can read it after
	// the fact instead of only being told at the moment it happened.
	result TaskResult
}

// AgentBuilder turns a template and a slot into a core agent.
//
// It is an interface because building an agent needs a provider and a tool set, and
// which provider and which tools is the assembly layer's decision. This package owns
// when a sub agent runs and what it is confined to, not how a model is reached.
type AgentBuilder interface {
	// Build makes the agent for one task. It is called once per task rather than once
	// per slot, because the agent is not reusable and pretending otherwise would put
	// two tasks in one conversation.
	Build(ctx context.Context, template Template, slot *Slot) (core.Agent, error)
}

// PooledAgentConfig configures a pool.
type PooledAgentConfig struct {
	// MainID is the main core's identifier. It is the only one allowed to act on a
	// slot, and it is required.
	MainID string
	// Registry owns the agents and the rule about who may create them.
	Registry *Registry
	// Broker carries the messages. A slot with no broker is a sub agent nobody can
	// talk to, which is a legitimate thing to build for a task that only reports back.
	Broker *Broker
	// Templates supplies the instructions and the tool set.
	Templates *TemplatePool
	// Builder makes the agent. A nil builder makes every task fail, which keeps the
	// pool testable without a model.
	Builder AgentBuilder
	// Capacity is how many slots may exist. Zero selects a small default, because a
	// pool with no bound is a way to exhaust a machine.
	Capacity int
	// MaxConcurrent is how many may run at once. Zero selects Capacity, and it is
	// separate from Capacity because a pool of eight slots that runs all eight at
	// once is a different thing from a pool of eight that runs two.
	MaxConcurrent int
}

const (
	defaultPoolCapacity      = 4
	defaultMaxTemplateLength = 64 << 10
)

// Pool is a bounded set of slots that sub agents run on.
//
// What is pooled is the slot, not the agent. The slot holds the identifier, the
// mailbox, the registry entry and the claim on a concurrency slot; the core agent on
// it is built per task and thrown away. That split is the whole design: a core agent
// has no way to clear its conversation, so an agent reused across tasks would answer
// the second question with the first task's context still in front of it, and calling
// that a sub agent with its own context would be the opposite of true.
type Pool struct {
	config  PooledAgentConfig
	mu      sync.RWMutex
	slots   map[string]*Slot
	order   []string
	running int
	// waiters is how many callers are blocked on a free slot, so a slot that frees up
	// knows there is somebody to hand it to.
	waiters chan struct{}
	closed  bool
	// nextID names the slots, and it only ever goes up so a slot keeps its identity
	// for the life of the pool.
	nextID int
}

// NewPool builds a pool.
func NewPool(config PooledAgentConfig) (*Pool, error) {
	if config.MainID == "" {
		return nil, fmt.Errorf("subagent: a pool needs the main core identifier")
	}
	capacity := config.Capacity
	if capacity == 0 {
		capacity = defaultPoolCapacity
	}
	if capacity < 1 {
		return nil, fmt.Errorf("subagent: a pool capacity must be positive")
	}
	concurrent := config.MaxConcurrent
	if concurrent == 0 {
		concurrent = capacity
	}
	if concurrent < 1 {
		return nil, fmt.Errorf("subagent: a concurrency limit must be positive")
	}
	if concurrent > capacity {
		return nil, fmt.Errorf(
			"subagent: a concurrency limit of %d is above the capacity of %d", concurrent, capacity)
	}
	return &Pool{
		config:  config,
		slots:   map[string]*Slot{},
		waiters: make(chan struct{}, 1),
	}, nil
}

// MainID reports the main core's identifier.
func (p *Pool) MainID() string { return p.config.MainID }

// Registry reports the registry, which is nil for a pool with no agents.
func (p *Pool) Registry() *Registry { return p.config.Registry }

// Broker reports the message broker.
func (p *Pool) Broker() *Broker { return p.config.Broker }

// Capacity reports how many slots the pool may hold.
func (p *Pool) Capacity() int { return p.config.Capacity }

// Stats reports what the pool is doing, which is what a caller watches to see
// whether it is the right size.
func (p *Pool) Stats() PoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	stats := PoolStats{
		Slots:     len(p.slots),
		Capacity:  p.config.Capacity,
		Running:   p.running,
		Available: p.config.Capacity - p.running,
	}
	// A slot's state is the sub agent's own, guarded by the slot's lock, not the
	// pool's. Counting them under the pool lock alone would read a field that a
	// running sub agent is writing, which is the kind of race that passes a hundred
	// runs and then hands someone a count that was never true.
	for _, slot := range p.slots {
		slot.mu.Lock()
		state := slot.State
		slot.mu.Unlock()
		switch state {
		case SlotBusy:
			stats.Busy++
		case SlotFailed:
			stats.Failed++
		case SlotStopped:
			stats.Stopped++
		}
	}
	return stats
}

// PoolStats is a snapshot of a pool.
type PoolStats struct {
	// Slots is how many exist.
	Slots int `json:"slots"`
	// Capacity is how many may exist.
	Capacity int `json:"capacity"`
	// Running is how many are running a task.
	Running int `json:"running"`
	// Busy, Failed and Stopped count the slots in each state.
	Busy    int `json:"busy"`
	Failed  int `json:"failed"`
	Stopped int `json:"stopped"`
	// Available is how many may start right now.
	Available int `json:"available"`
	// Queued is how many callers are waiting for a slot.
	Queued int `json:"queued"`
}

// CreateSlot makes a slot, which only the main core may do.
//
// The rule is enforced here as well as by the registry, because a slot is what a
// sub agent runs on and a slot created by a sub agent is a machine resource that
// nobody bounded.
func (p *Pool) CreateSlot(ctx context.Context, byAgentID, template string) (SlotRecord, error) {
	if ctx == nil {
		return SlotRecord{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return SlotRecord{}, err
	}
	if byAgentID != p.config.MainID {
		return SlotRecord{}, fmt.Errorf("%w: %q tried to create a slot", ErrNotMain, byAgentID)
	}
	template = strings.TrimSpace(template)
	if template == "" {
		return SlotRecord{}, fmt.Errorf("subagent: a slot needs a template")
	}
	if p.config.Templates != nil {
		if _, ok := p.config.Templates.Lookup(template); !ok {
			return SlotRecord{}, fmt.Errorf("%w: %s", ErrTemplateNotFound, template)
		}
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return SlotRecord{}, ErrPoolClosed
	}
	if len(p.slots) >= p.config.Capacity {
		p.mu.Unlock()
		return SlotRecord{}, fmt.Errorf("%w: the pool already holds its %d slots", ErrPoolFull, p.config.Capacity)
	}
	p.nextID++
	slot := &Slot{
		SlotRecord: SlotRecord{
			ID:        fmt.Sprintf("slot-%d", p.nextID),
			Template:  template,
			State:     SlotIdle,
			CreatedAt: timeNow(),
			UpdatedAt: timeNow(),
		},
		pool: p,
	}
	p.slots[slot.ID] = slot
	p.order = append(p.order, slot.ID)
	p.mu.Unlock()
	// The slot is registered before it is handed back, because a slot the main core
	// cannot see is a sub agent it cannot ask about, and the registry is where a
	// caller looks to find out what exists. The tools are filed from the template's
	// entry, so the record says what this sub agent may reach without anybody
	// reading its prompt.
	if p.config.Registry != nil {
		entry, _ := p.config.Templates.Lookup(template)
		if _, err := p.config.Registry.Register(ctx, p.config.MainID, Spec{
			Name:         slot.ID,
			AgentID:      slot.ID,
			SystemPrompt: "",
			Tools:        entry.Tools,
			Metadata:     map[string]any{"template": template, "slot": slot.ID},
		}); err != nil {
			// A slot that cannot be registered would be invisible, so it is not kept.
			p.mu.Lock()
			delete(p.slots, slot.ID)
			for index, id := range p.order {
				if id == slot.ID {
					p.order = append(p.order[:index], p.order[index+1:]...)
					break
				}
			}
			p.mu.Unlock()
			return SlotRecord{}, fmt.Errorf("subagent: register the slot %s: %w", slot.ID, err)
		}
	}
	return slot.snapshot(), nil
}

// GetSlot returns a slot's state.
func (p *Pool) GetSlot(slotID string) (SlotRecord, bool) {
	p.mu.RLock()
	slot, ok := p.slots[slotID]
	p.mu.RUnlock()
	if !ok {
		return SlotRecord{}, false
	}
	return slot.snapshot(), true
}

// ListSlots returns every slot, in the order they were created.
func (p *Pool) ListSlots() []SlotRecord {
	p.mu.RLock()
	slots := make([]*Slot, 0, len(p.slots))
	for _, id := range p.order {
		if slot, ok := p.slots[id]; ok {
			slots = append(slots, slot)
		}
	}
	p.mu.RUnlock()
	out := make([]SlotRecord, 0, len(slots))
	for _, slot := range slots {
		out = append(out, slot.snapshot())
	}
	return out
}

// DeleteSlot removes a slot, which only the main core may do.
//
// A busy slot is refused rather than killed. A task that is mid tool call does not
// stop when its slot disappears, and a sub agent whose slot has been deleted is a
// sub agent writing into a mailbox nobody is reading.
func (p *Pool) DeleteSlot(ctx context.Context, byAgentID, slotID string) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if byAgentID != p.config.MainID {
		return fmt.Errorf("%w: %q tried to delete the slot %q", ErrNotMain, byAgentID, slotID)
	}
	p.mu.Lock()
	slot, ok := p.slots[slotID]
	if !ok {
		p.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoSlot, slotID)
	}
	if slot.State == SlotBusy {
		p.mu.Unlock()
		return fmt.Errorf("subagent: the slot %q is running a task", slotID)
	}
	delete(p.slots, slotID)
	for index, id := range p.order {
		if id == slotID {
			p.order = append(p.order[:index], p.order[index+1:]...)
			break
		}
	}
	p.mu.Unlock()
	if p.config.Registry != nil {
		_ = p.config.Registry.Delete(ctx, p.config.MainID, slot.ID)
	}
	return nil
}

// StopSlot stops a slot's current task, which only the main core may do.
//
// Stopping is separate from deleting because they answer different questions. A
// caller that stops a slot wants the work to end and the slot to survive; a caller
// that deletes one wants it gone.
func (p *Pool) StopSlot(ctx context.Context, byAgentID, slotID string) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if byAgentID != p.config.MainID {
		return fmt.Errorf("%w: %q tried to stop the slot %q", ErrNotMain, byAgentID, slotID)
	}
	p.mu.RLock()
	slot, ok := p.slots[slotID]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSlot, slotID)
	}
	slot.mu.Lock()
	cancel := slot.cancel
	slot.mu.Unlock()
	if cancel != nil {
		// The task's own context is cancelled rather than the caller's, so stopping a
		// slot is the one thing that does not stop the person who asked for it.
		cancel()
	}
	slot.markStopped()
	return nil
}

// StartSlot returns a stopped slot to service.
func (p *Pool) StartSlot(ctx context.Context, byAgentID, slotID string) (SlotRecord, error) {
	if ctx == nil {
		return SlotRecord{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return SlotRecord{}, err
	}
	if byAgentID != p.config.MainID {
		return SlotRecord{}, fmt.Errorf("%w: %q tried to start the slot %q", ErrNotMain, byAgentID, slotID)
	}
	p.mu.RLock()
	slot, ok := p.slots[slotID]
	p.mu.RUnlock()
	if !ok {
		return SlotRecord{}, fmt.Errorf("%w: %s", ErrNoSlot, slotID)
	}
	slot.markIdle()
	return slot.snapshot(), nil
}

// Close stops every slot and refuses anything after.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	slots := make([]*Slot, 0, len(p.slots))
	for _, slot := range p.slots {
		slots = append(slots, slot)
	}
	p.mu.Unlock()
	for _, slot := range slots {
		slot.mu.Lock()
		cancel := slot.cancel
		slot.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return nil
}

// ErrPoolClosed is a request against a pool that has been closed.
var ErrPoolClosed = errors.New("subagent: the pool is closed")

// eventBuffer is how many events queue for the main core before the sub agent has to
// wait for it. It is generous because a sub agent that stalls waiting for a slow
// watcher is a sub agent wasting the slot it was given.
const eventBuffer = 64

// Start puts a task on a free slot and runs it in its own goroutine, returning as
// soon as it is under way.
//
// One sub agent is one goroutine, and that is the whole reason a caller is not made
// to wait. The main core starts the work it wants done and carries on with its own
// turn; the sub agent runs its own loop on its own context, and the result arrives
// through the message broker like any other message. A pool that blocked its caller
// would be a pool with a concurrency limit nobody could use, because the caller
// would only ever have one sub agent in flight.
//
// Starting is separate from creating for the same reason. A slot is a place, a task
// is work, and the main core may put several tasks on several slots and then read
// their state, read their messages and stop any of them while they are still going.
func (p *Pool) Start(ctx context.Context, byAgentID string, task Task) (SlotRecord, error) {
	if ctx == nil {
		return SlotRecord{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return SlotRecord{}, err
	}
	if byAgentID != p.config.MainID {
		return SlotRecord{}, fmt.Errorf("%w: %q tried to start a task", ErrNotMain, byAgentID)
	}
	if p.config.Builder == nil {
		return SlotRecord{}, fmt.Errorf(
			"subagent: no agent builder is configured, so nothing can run")
	}
	slot, release, err := p.claim(ctx, task.Slot)
	if err != nil {
		return SlotRecord{}, err
	}
	loaded, err := p.templateFor(ctx, slot.Template)
	if err != nil {
		release()
		return SlotRecord{}, err
	}
	// The task's context is the caller's, so a caller that gives up takes the sub
	// agent with it, and a slot that is stopped cancels this rather than the caller.
	runCtx, cancel := context.WithCancel(ctx)
	events := make(chan core.Event, eventBuffer)
	finished := make(chan struct{})

	slot.mu.Lock()
	slot.cancel = cancel
	slot.events = events
	slot.finished = finished
	slot.result = TaskResult{}
	slot.State = SlotBusy
	slot.UpdatedAt = timeNow()
	slot.mu.Unlock()

	// The slot is released whatever happens, because a slot left busy is a slot the
	// pool can never use again and a pool that shrinks under load is worse than one
	// that reports it is full.
	go func() {
		defer release()
		defer close(finished)
		defer func() {
			cancel()
			slot.mu.Lock()
			slot.cancel = nil
			slot.events = nil
			slot.finished = nil
			slot.agent = nil
			slot.mu.Unlock()
			// Whatever the watchers did not take is emptied, and then the channel is
			// closed. Closing is the part that matters: a caller ranging over a slot's
			// events is waiting for the task to end, and a channel that is emptied but
			// never closed leaves it waiting for a task that is already over.
			for {
				select {
				case _, open := <-events:
					if !open {
						return
					}
				default:
					close(events)
					return
				}
			}
		}()
		result := p.execute(runCtx, slot, loaded, task, events)
		slot.mu.Lock()
		slot.result = result
		slot.mu.Unlock()
	}()
	return slot.snapshot(), nil
}

// execute runs one task to completion and files what it produced.
//
// It runs on the sub agent's own goroutine. Everything it touches is either the
// slot, which is busy for the duration, or state the main core is allowed to read.
func (p *Pool) execute(ctx context.Context, slot *Slot, template Template, task Task, watchers chan<- core.Event) TaskResult {
	agent, err := p.config.Builder.Build(ctx, template, slot)
	if err != nil {
		failure := fmt.Errorf("subagent: build the agent for %s: %w", slot.ID, err)
		slot.recordFailure(task, failure, 0)
		return TaskResult{
			Slot: slot.ID, AgentID: slot.ID, Summary: task.Summary,
			State: StateFailed, Err: failure.Error(),
		}
	}
	slot.mu.Lock()
	slot.agent = agent
	slot.mu.Unlock()

	started := timeNow()
	stream, err := agent.Run(ctx, task.Input)
	if err != nil {
		failure := fmt.Errorf("subagent: run on %s: %w", slot.ID, err)
		slot.recordFailure(task, failure, timeNow().Sub(started))
		return TaskResult{
			Slot: slot.ID, AgentID: slot.ID, Summary: task.Summary,
			State: StateFailed, Duration: timeNow().Sub(started), Err: failure.Error(),
		}
	}
	failure := p.drain(slot, stream, watchers)
	elapsed := timeNow().Sub(started)
	if failure == nil && ctx.Err() != nil {
		// The sub agent was stopped, or the caller gave up, part way through. The
		// stream ended tidily, so nothing in it says so, but a task that was cut off
		// did not finish its work and must not be filed as though it had.
		failure = fmt.Errorf("subagent: %s was stopped before it finished: %w", slot.ID, ctx.Err())
	}

	result := TaskResult{
		Slot:     slot.ID,
		AgentID:  slot.ID,
		Summary:  task.Summary,
		State:    StateDone,
		Messages: slot.messageCount(),
		Duration: elapsed,
	}
	if failure != nil {
		result.State = StateFailed
		result.Err = failure.Error()
		slot.recordFailure(task, failure, elapsed)
		return result
	}
	if err := p.settle(ctx, slot, template, task, result); err != nil {
		result.State = StateFailed
		result.Err = err.Error()
		slot.recordFailure(task, err, elapsed)
		return result
	}
	slot.recordSuccess(task, elapsed)
	return result
}

// Wait blocks until a slot's task finishes and reports what it produced.
//
// It is here for the callers that genuinely need the answer: a test, and the
// sequential shape of an orchestration where the next step depends on what this one
// found. A caller that does not need it uses Start and reads the result later, or
// waits for the message.
func (p *Pool) Wait(ctx context.Context, slotID string) (TaskResult, error) {
	if ctx == nil {
		return TaskResult{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return TaskResult{}, err
	}
	p.mu.RLock()
	slot, ok := p.slots[slotID]
	p.mu.RUnlock()
	if !ok {
		return TaskResult{}, fmt.Errorf("%w: %s", ErrNoSlot, slotID)
	}
	slot.mu.Lock()
	finished := slot.finished
	result := slot.result
	slot.mu.Unlock()
	if finished == nil {
		// Nothing is running, so whatever the last task produced is the answer. A
		// caller waiting for a task that will never start would wait forever, and this
		// is the state a slot is in between tasks.
		if result.Err != "" {
			return result, errors.New(result.Err)
		}
		return result, nil
	}
	select {
	case <-finished:
		slot.mu.Lock()
		result := slot.result
		slot.mu.Unlock()
		if result.Err != "" {
			return result, errors.New(result.Err)
		}
		return result, nil
	case <-ctx.Done():
		return TaskResult{}, ctx.Err()
	}
}

// Watch hands back the events of whatever task is on a slot right now.
//
// A slot with no task gets a channel that is already closed rather than an error,
// because the caller almost always wants the same loop either way and an error for
// the ordinary case of looking at an idle slot would make every caller handle a
// condition that is not a problem.
func (p *Pool) Watch(slotID string) (<-chan core.Event, bool) {
	p.mu.RLock()
	slot, ok := p.slots[slotID]
	p.mu.RUnlock()
	if !ok {
		closed := make(chan core.Event)
		close(closed)
		return closed, false
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.events == nil {
		closed := make(chan core.Event)
		close(closed)
		return closed, true
	}
	return slot.events, true
}

// Result reports a slot's last task without waiting, so a caller can ask what has
// happened so far.
func (p *Pool) Result(slotID string) (TaskResult, bool) {
	p.mu.RLock()
	slot, ok := p.slots[slotID]
	p.mu.RUnlock()
	if !ok {
		return TaskResult{}, false
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.result, slot.Runs > 0
}

// templateFor reads a slot's template off disk, at the moment the task starts.
//
// It is read here rather than when the slot was created because a template is a file
// the main core may have rewritten in between, and a sub agent that ran on yesterday's
// copy of its instructions would be a sub agent nobody could account for.
func (p *Pool) templateFor(ctx context.Context, name string) (Template, error) {
	if p.config.Templates == nil {
		return Template{}, fmt.Errorf("subagent: no template pool is configured, so nothing can run")
	}
	template, err := p.config.Templates.Load(ctx, name)
	if err != nil {
		return Template{}, err
	}
	return template, nil
}

// settle records what a finished task produced, so the main core can find it later
// from either side: the registry, which is where it looks for state, and the broker,
// which is how the answer reaches it.
//
// Both are written before the slot is freed. A result that lived only in the caller's
// hands would be a result the main core never sees, and the main core is the only one
// allowed to start the next thing.
func (p *Pool) settle(ctx context.Context, slot *Slot, template Template, task Task, result TaskResult) error {
	summary := result.Summary
	if p.config.Registry != nil {
		// The summary is filed even when it is empty, because a caller that asked for
		// something and got nothing back deserves to find that written down rather
		// than left to infer it from an unchanged record.
		if err := p.config.Registry.SetSummary(slot.ID, summary); err != nil {
			return fmt.Errorf("subagent: file the summary for %s: %w", slot.ID, err)
		}
		if err := p.config.Registry.SetState(slot.ID, StateDone); err != nil {
			return fmt.Errorf("subagent: file the state for %s: %w", slot.ID, err)
		}
	}
	if p.config.Broker == nil {
		return nil
	}
	if _, err := p.config.Broker.ToMain(ctx, Message{
		FromAgentID: slot.ID,
		ToAgentID:   p.config.MainID,
		Type:        "result",
		Content:     summary,
		Data:        taskResultData(result),
	}); err != nil {
		return fmt.Errorf("subagent: report the result of %s to the main core: %w", slot.ID, err)
	}
	return nil
}

// taskResultData renders a result as the message body. It is a function rather than a
// field marshal because a result carries a duration and a slot, and the reader on the
// other end wants the summary first and the details after.
func taskResultData(result TaskResult) []byte {
	encoded, err := json.Marshal(result)
	if err != nil {
		// A result is only strings, a duration and a count, so this cannot fail in
		// practice. Reporting the failure is still better than sending a message with
		// a body nobody can read.
		return []byte(`{"state":"` + result.State + `"}`)
	}
	return encoded
}

// drain consumes a sub agent's event stream, hands the events to whoever is watching,
// and reports the first failure it saw.
//
// The stream is drained to the end even after an error, because a sub agent that has
// produced an error still has a half-finished turn to put away, and a slot freed
// mid-stream would be a slot whose agent is still writing.
func (p *Pool) drain(slot *Slot, stream <-chan core.Event, watchers chan<- core.Event) error {
	var firstErr error
	for event := range stream {
		if event.Type == "error" && firstErr == nil {
			if event.Error != "" {
				firstErr = fmt.Errorf("subagent: %s failed: %s", slot.ID, event.Error)
			} else {
				// The event said it was an error without saying what. Recording that much
				// is better than passing the failure on as a success.
				firstErr = fmt.Errorf("subagent: %s failed without saying why", slot.ID)
			}
		}
		if watchers == nil {
			continue
		}
		select {
		case watchers <- event:
		case <-time.After(watchTimeout):
			// A watcher that is not keeping up loses events rather than stalling the
			// sub agent. The main core can always read the result and the messages;
			// a stalled slot cannot be read at all.
		}
	}
	return firstErr
}

// watchTimeout is how long a sub agent will hold up its own work waiting for a slow
// watcher to take an event.
const watchTimeout = 2 * time.Second

// recordSuccess returns a slot to service after a task that worked, and leaves behind
// what the task was and how long it took, so a slot that has run three times reads as
// a slot with a history rather than three anonymous runs.
func (s *Slot) recordSuccess(task Task, elapsed time.Duration) {
	s.mu.Lock()
	s.Runs++
	s.UpdatedAt = timeNow()
	s.LastSummary = task.Summary
	s.LastErr = ""
	s.LastDuration = elapsed
	s.State = SlotIdle
	s.mu.Unlock()
	// The registry is not touched here. Settle already recorded the finished state and
	// the summary, and writing it twice would mean the second write could disagree with
	// the first.
}

// recordFailure returns a slot to service after a task that did not work, and says
// why, so the next caller can see that this slot ran something that failed rather
// than nothing at all.
//
// A failed slot is idle again rather than stopped. It failed, it did not break, and a
// slot that went unusable on one failure would make a pool of four survive exactly one
// bad task.
func (s *Slot) recordFailure(task Task, failure error, elapsed time.Duration) {
	s.mu.Lock()
	s.Runs++
	s.UpdatedAt = timeNow()
	s.LastSummary = task.Summary
	s.LastErr = failure.Error()
	s.LastDuration = elapsed
	if s.State != SlotStopped {
		s.State = SlotFailed
	}
	s.mu.Unlock()
	if s.pool == nil || s.pool.config.Registry == nil {
		return
	}
	_ = s.pool.config.Registry.SetState(s.ID, StateFailed)
	_ = s.pool.config.Registry.SetSummary(s.ID, failure.Error())
}

// messageCount reports how many messages this slot's mailbox has taken.
//
// It counts what was delivered. It deliberately does not count what was dropped,
// because a drop is a fault the broker already records, and a reader that took drops
// for traffic would conclude the sub agent had been sent more than it had.
func (s *Slot) messageCount() int64 {
	if s.pool == nil || s.pool.config.Broker == nil {
		return 0
	}
	mailbox, ok := s.pool.config.Broker.Mailbox(s.ID)
	if !ok {
		return 0
	}
	return mailbox.Delivered()
}

// claim takes a free slot, waiting for one if the caller is willing to wait.
func (p *Pool) claim(ctx context.Context, wanted string) (*Slot, func(), error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, nil, ErrPoolClosed
		}
		slot, ok := p.claimLocked(wanted)
		if ok {
			p.mu.Unlock()
			return slot, func() { p.release(slot) }, nil
		}
		// A named slot that will never be free is a refusal, not a queue. Waiting for
		// a stopped slot would wait for good, and a caller that asked for a particular
		// place to work has to be told that place is closed rather than left waiting
		// for a moment that cannot arrive.
		if wanted != "" {
			if named, exists := p.slots[wanted]; exists {
				named.mu.Lock()
				state := named.State
				named.mu.Unlock()
				if state == SlotStopped {
					p.mu.Unlock()
					return nil, nil, fmt.Errorf(
						"%w: the slot %s was stopped, so it will not take work", ErrSlotStopped, wanted)
				}
			}
		}
		// Work offered to a pool with no slots is refused for the same reason. Slots
		// are made by the main core asking for them, so an empty pool will not fill
		// itself, and a caller left waiting for one would be waiting for something
		// only it can bring.
		if len(p.slots) == 0 {
			p.mu.Unlock()
			return nil, nil, fmt.Errorf(
				"%w: the pool has no slots, so create one before offering work", ErrNoSlots)
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-p.waiters:
			// A slot may have been freed. A pool with no waiters and no free slot is a
			// pool at its concurrency limit with nobody asking, which cannot happen;
			// if it somehow does, waiting again is the only honest answer.
		case <-time.After(waitPoll):
		}
	}
}

const waitPoll = 5 * time.Millisecond

// claimLocked finds a slot and takes the concurrency claim. The pool's lock is held.
func (p *Pool) claimLocked(wanted string) (*Slot, bool) {
	if p.running >= p.config.MaxConcurrent {
		return nil, false
	}
	ids := p.order
	if wanted != "" {
		ids = []string{wanted}
	}
	for _, id := range ids {
		slot, ok := p.slots[id]
		if !ok {
			continue
		}
		slot.mu.Lock()
		usable := slot.State == SlotIdle || slot.State == SlotFailed
		if usable {
			// The slot is marked busy here rather than by the caller once it starts
			// work, because between those two moments a second caller would find the
			// slot idle and take it too. Two tasks on one slot is two conversations in
			// one, and a slot deleted in that window is a task writing into nothing.
			slot.State = SlotBusy
			slot.UpdatedAt = timeNow()
		}
		slot.mu.Unlock()
		if !usable {
			continue
		}
		p.running++
		return slot, true
	}
	return nil, false
}

// release gives a slot's concurrency claim back and wakes somebody waiting for it.
func (p *Pool) release(slot *Slot) {
	// A claim is only half the slot. Claiming marks the slot busy so nothing else can
	// take it, so freeing the claim has to undo that too, or a task that failed before
	// it ever reported an outcome would take the slot out of service for good. A pool
	// of four that lost one to a bad template would lose the next to a panic.
	slot.mu.Lock()
	if slot.State == SlotBusy {
		slot.State = SlotIdle
		slot.UpdatedAt = timeNow()
	}
	slot.mu.Unlock()

	p.mu.Lock()
	if p.running > 0 {
		p.running--
	}
	p.mu.Unlock()
	select {
	case p.waiters <- struct{}{}:
	default:
	}
}

// snapshot returns the slot's state under its own lock, taken before the pool's, so
// two locks are never held at once in one direction.
func (s *Slot) snapshot() SlotRecord {
	s.mu.Lock()
	state := SlotRecord{
		ID:           s.ID,
		Template:     s.Template,
		State:        s.State,
		Runs:         s.Runs,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
		LastSummary:  s.LastSummary,
		LastErr:      s.LastErr,
		LastDuration: s.LastDuration,
	}
	s.mu.Unlock()
	return state
}

func (s *Slot) markStopped() {
	s.mu.Lock()
	if s.State != SlotBusy {
		s.State = SlotStopped
		s.UpdatedAt = timeNow()
	}
	s.mu.Unlock()
}

func (s *Slot) markIdle() {
	s.mu.Lock()
	if s.State != SlotBusy {
		s.State = SlotIdle
		s.UpdatedAt = timeNow()
	}
	s.mu.Unlock()
}
