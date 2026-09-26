// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrNotAllowed is a read or an act that the caller has no standing to make.
var ErrNotAllowed = errors.New("subagent: not allowed")

// AgentView is everything there is to know about one sub agent at one moment.
//
// It is one record rather than two because the pool and the registry each hold half
// the truth. The registry knows what a sub agent was asked to do and what it said it
// did; the pool knows whether it is running right now and how many times it has been
// used. A caller reading one and not the other would be told half a story, and the
// half it is missing is the half that changes.
type AgentView struct {
	// ID is the sub agent's identifier, which is also its slot's.
	ID string `json:"id"`
	// Name is its label.
	Name string `json:"name,omitempty"`
	// Template is the file its instructions come from.
	Template string `json:"template,omitempty"`
	// State is where it is in its life, as the pool sees it live.
	State SlotState `json:"state"`
	// Recorded is what the registry was told, which is the durable half. It can lag
	// the live state by the length of one task, and it is kept anyway because it is
	// what survives a restart and what an audit reads.
	Recorded State `json:"recorded_state"`
	// Tools is what it may use, which its template decided and nobody else may widen.
	Tools []string `json:"tools,omitempty"`
	// Summary is what it was last asked to do and what it made of it.
	Summary string `json:"summary,omitempty"`
	// Err is why the last task failed.
	Err string `json:"error,omitempty"`
	// Runs is how many tasks this slot has run, which is what makes reuse visible.
	Runs int64 `json:"runs"`
	// LastDuration is how long the last task took.
	LastDuration time.Duration `json:"last_duration"`
	// Delivered is how many messages have reached this sub agent since it was
	// created, and Waiting is how many it has not read yet. The first is the
	// conversation, the second is the backlog.
	Delivered int64 `json:"delivered"`
	Waiting   int   `json:"waiting"`
	// Dropped is how many messages it could not be given because it was not reading.
	// It is a fault, and it is reported rather than hidden because a sub agent that
	// silently missed instructions will simply do the wrong work.
	Dropped int64 `json:"dropped"`
	// CreatedAt and UpdatedAt bound its life.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Running reports whether a task is on this sub agent right now.
func (v AgentView) Running() bool { return v.State == SlotBusy }

// View is the whole picture at one moment.
type View struct {
	// MainID is the main core this view belongs to. It is not in SubAgents, because
	// the main core is not its own sub agent and a list that included it would have
	// to be filtered by every reader.
	MainID string `json:"main_id"`
	// Capacity is how many slots the pool may hold.
	Capacity int `json:"capacity"`
	// SubAgents is every sub agent that exists, in a stable order.
	SubAgents []AgentView `json:"sub_agents"`
}

// Get returns one sub agent's view by name.
func (v View) Get(agentID string) (AgentView, bool) {
	for _, agent := range v.SubAgents {
		if agent.ID == agentID {
			return agent, true
		}
	}
	return AgentView{}, false
}

// Summary counts the sub agents by state, so a caller can see at a glance whether
// anything is stuck rather than reading the list and counting it by eye.
func (v View) Summary() map[SlotState]int {
	counts := make(map[SlotState]int, len(v.SubAgents))
	for _, agent := range v.SubAgents {
		counts[agent.State]++
	}
	return counts
}

// View gathers what is true about every sub agent right now.
//
// It takes nothing out of anything. A caller asking what is going on must not end up
// having consumed the answer, or the second question would find an empty mailbox and
// the sub agent would look idle when it is merely unheard. Reading a message is a
// separate, deliberate act, and it is Take.
func (p *Pool) View() View {
	view := View{MainID: p.config.MainID, Capacity: p.config.Capacity}

	// The pool's own lock is taken to copy the slot list and released before any
	// slot's lock is taken, so the two are never held at once. The registry is read
	// afterwards, and separately, for the same reason: a lock order between two
	// structures is a deadlock waiting for the one caller that takes them the other
	// way round.
	p.mu.RLock()
	slots := make([]SlotRecord, 0, len(p.order))
	for _, id := range p.order {
		if slot, ok := p.slots[id]; ok {
			slots = append(slots, slot.snapshot())
		}
	}
	p.mu.RUnlock()

	recorded := make(map[string]Agent)
	if p.config.Registry != nil {
		for _, agent := range p.config.Registry.List() {
			if !agent.IsMain() {
				recorded[agent.ID] = agent
			}
		}
	}

	view.SubAgents = make([]AgentView, 0, len(slots))
	for _, slot := range slots {
		entry := AgentView{
			ID:           slot.ID,
			Template:     slot.Template,
			State:        slot.State,
			Summary:      slot.LastSummary,
			Err:          slot.LastErr,
			Runs:         slot.Runs,
			LastDuration: slot.LastDuration,
			CreatedAt:    slot.CreatedAt,
			UpdatedAt:    slot.UpdatedAt,
		}
		// The tools are only in the registry, because the template is what decided
		// them and the registry is where a template's decision was filed. Reading them
		// from there rather than copying them onto the slot keeps one place that
		// decides what a sub agent may reach.
		if agent, ok := recorded[slot.ID]; ok {
			entry.Name = agent.Name
			entry.Recorded = agent.State
			entry.Tools = append([]string(nil), agent.Tools...)
			// The summary the registry holds wins when the slot has none, because a
			// sub agent that was told what it did should say so even if the pool lost
			// the thread.
			if entry.Summary == "" {
				entry.Summary = agent.Summary
			}
			if entry.Err == "" {
				entry.Err = agent.Err
			}
		}
		if p.config.Broker != nil {
			if mailbox, ok := p.config.Broker.Mailbox(slot.ID); ok {
				entry.Delivered = mailbox.Delivered()
				entry.Waiting = mailbox.Len()
				entry.Dropped = mailbox.Dropped()
			}
		}
		view.SubAgents = append(view.SubAgents, entry)
	}
	// A stable order means two views of the same state can be compared, and a
	// main core reading a list twice does not see the sub agents move about for no
	// reason.
	sort.Slice(view.SubAgents, func(i, j int) bool {
		return view.SubAgents[i].ID < view.SubAgents[j].ID
	})
	return view
}

// Take removes up to limit messages from a sub agent's mailbox and hands them back.
//
// It is how a sub agent collects what it was sent, and how the main core collects a
// result it would rather not wait for. Taking is not reading: a message handed back
// here is gone from the mailbox, so a caller that only wanted to look has the View.
//
// A sub agent may take from its own mailbox and from nobody else's. The main core may
// take from any, because collecting a sub agent's output is the whole reason it gave
// the work out. That asymmetry is the rule: a sub agent that could drain another
// sub agent's mailbox would be able to swallow a result meant for the main core.
func (p *Pool) Take(ctx context.Context, byAgentID, agentID string, limit int) ([]Message, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if byAgentID != p.config.MainID && byAgentID != agentID {
		return nil, fmt.Errorf("%w: %q took messages from %q", ErrNotAllowed, byAgentID, agentID)
	}
	if p.config.Broker == nil {
		return nil, fmt.Errorf("subagent: the pool has no broker, so there are no messages")
	}
	mailbox, ok := p.config.Broker.Mailbox(agentID)
	if !ok {
		return nil, fmt.Errorf("%w: %q has no mailbox", ErrNoSlot, agentID)
	}
	// No limit means everything waiting, which is what a sub agent draining its own
	// work wants. A limit of zero would be a caller asking for nothing, which is
	// almost certainly a mistake rather than an intention, so it is refused.
	if limit < 0 {
		return nil, fmt.Errorf("subagent: a limit of %d cannot be read", limit)
	}
	if limit == 0 {
		limit = mailbox.Len()
	}
	taken := make([]Message, 0, limit)
	for range limit {
		// The non-blocking read, not Receive. A caller that asked for ten messages
		// and there are three would otherwise sit on the fourth for ever, waiting for
		// a message nobody has promised to send.
		message, ok := mailbox.ReceiveWithin(0)
		if !ok {
			break
		}
		taken = append(taken, message)
	}
	return taken, nil
}

// WaitMessages waits for a message to arrive for a sub agent, which is how a sub
// agent that has run out of work blocks until the main core gives it some.
//
// It is the one place in this package where waiting is right, because the arrival of
// a message is genuinely something the caller cannot predict and can safely be
// interrupted by a deadline or a cancellation.
func (p *Pool) WaitMessages(ctx context.Context, agentID string, wait time.Duration) (Message, error) {
	if ctx == nil {
		return Message{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	if p.config.Broker == nil {
		return Message{}, fmt.Errorf("subagent: the pool has no broker, so there are no messages")
	}
	mailbox, ok := p.config.Broker.Mailbox(agentID)
	if !ok {
		return Message{}, fmt.Errorf("%w: %q has no mailbox", ErrNoSlot, agentID)
	}
	if wait <= 0 {
		// A caller that will not wait gets what is there, because the alternative is
		// to block for ever on a message that may never be sent. ReceiveWithin with no
		// wait is the non-blocking read; Receive is not, and using it here would turn
		// a request not to wait into a request to wait for good.
		if message, ok := mailbox.ReceiveWithin(0); ok {
			return message, nil
		}
		return Message{}, fmt.Errorf("subagent: %q has no message waiting", agentID)
	}
	// The caller's own cancellation is passed down, so a sub agent waiting for work is
	// let go the moment its turn ends rather than when the wait it asked for expires.
	message, ok := mailbox.ReceiveWithinContext(ctx, wait)
	if !ok {
		if err := ctx.Err(); err != nil {
			return Message{}, err
		}
		return Message{}, fmt.Errorf("subagent: no message for %q within %s", agentID, wait)
	}
	return message, nil
}
