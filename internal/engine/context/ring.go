// SPDX-License-Identifier: Apache-2.0

package context

import (
	"fmt"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// OverflowHandler is told when the ring buffer has to drop something.
//
// It exists because dropping history is not something to do quietly: a
// conversation that silently lost its oldest turns would look to the user like a
// model that forgot, which is a far worse failure than a reported overflow.
type OverflowHandler interface {
	OnOverflow(dropped int, totalDropped int)
}

// OverflowHandlerFunc adapts a function to OverflowHandler.
type OverflowHandlerFunc func(dropped int, totalDropped int)

// OnOverflow calls the function.
func (f OverflowHandlerFunc) OnOverflow(dropped int, totalDropped int) { f(dropped, totalDropped) }

// RingBuffer keeps the most recent messages, dropping the oldest when it is
// full.
//
// The design sets a single message cap, a ring buffer and an overflow alarm as
// three separate defences: the cap stops one oversized message, compaction stops
// an oversized conversation, and the ring stops a conversation that grows without
// bound because nothing is consuming it. Which one fires depends on the
// assembly, so all three exist here.
type RingBuffer struct {
	mu       sync.RWMutex
	messages []core.Message
	capacity int
	dropped  int
	handler  OverflowHandler
}

// NewRingBuffer builds a ring buffer.
func NewRingBuffer(capacity int, handler OverflowHandler) (*RingBuffer, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("context: the ring capacity must be positive, got %d", capacity)
	}
	return &RingBuffer{capacity: capacity, handler: handler}, nil
}

// Append adds a message, dropping the oldest ones if the buffer is full.
func (r *RingBuffer) Append(messages ...core.Message) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, messages...)
	if len(r.messages) <= r.capacity {
		return 0
	}
	dropped := len(r.messages) - r.capacity
	// Copy the surviving tail into a fresh slice, so the buffer does not keep
	// the dropped messages alive through the backing array.
	surviving := make([]core.Message, r.capacity)
	copy(surviving, r.messages[dropped:])
	r.messages = surviving
	r.dropped += dropped
	if r.handler != nil {
		handler, total := r.handler, r.dropped
		// The handler runs outside the lock by way of a local copy, because it
		// may log or emit, and holding the lock across that would let a slow
		// handler block every append.
		go handler.OnOverflow(dropped, total)
	}
	return dropped
}

// Messages returns the retained messages, oldest first.
//
// The messages and their content blocks are copied, not just the slice. A shallow
// copy would let a caller reach into the buffer's own blocks through the content
// slice, which is the same state a later append would append to.
func (r *RingBuffer) Messages() []core.Message {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]core.Message, 0, len(r.messages))
	for _, message := range r.messages {
		out = append(out, cloneMessage(message))
	}
	return out
}

// cloneMessage copies a message deeply enough that a caller cannot change the
// original through it.
func cloneMessage(message core.Message) core.Message {
	copied := message
	if message.Content != nil {
		copied.Content = make([]core.ContentBlock, len(message.Content))
		copy(copied.Content, message.Content)
	}
	return copied
}

// Len reports how many messages are retained.
func (r *RingBuffer) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.messages)
}

// Dropped reports how many messages have been dropped since the buffer was
// created.
func (r *RingBuffer) Dropped() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dropped
}

// Capacity reports the configured capacity.
func (r *RingBuffer) Capacity() int { return r.capacity }

// Reset empties the buffer, keeping the drop count because a reset is not a
// reason to forget that history was lost.
func (r *RingBuffer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = nil
}
