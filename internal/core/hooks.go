// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
)

const (
	// HookPreLoop runs before a user message is appended.
	HookPreLoop HookType = "pre_loop"
	// HookPostLoop runs after the reasoning and tool loop settles.
	HookPostLoop HookType = "post_loop"
	// HookPreModelCall runs before each provider request is built.
	HookPreModelCall HookType = "pre_model_call"
	// HookPostModelCall runs after each provider stream is consumed.
	HookPostModelCall HookType = "post_model_call"
	// HookToolCallReceived runs before tool calls are accepted.
	HookToolCallReceived HookType = "tool_call_received"
	// HookPreToolExec runs before a tool invocation is executed.
	HookPreToolExec HookType = "pre_tool_exec"
	// HookPostToolExec runs after all tools in a batch settle.
	HookPostToolExec HookType = "post_tool_exec"
	// HookToolResult runs once for each produced tool result.
	HookToolResult HookType = "tool_result"
	// HookPreCompact runs before context compaction.
	HookPreCompact HookType = "pre_compact"
	// HookPostCompact runs after context compaction.
	HookPostCompact HookType = "post_compact"
	// HookContextInject runs while a model request is assembled.
	HookContextInject HookType = "context_inject"
	// HookSubAgentSpawn runs before a sub-agent is created.
	HookSubAgentSpawn HookType = "sub_agent_spawn"
	// HookSubAgentDone runs after a sub-agent settles.
	HookSubAgentDone HookType = "sub_agent_done"
	// HookOnEvent asynchronously observes immutable events.
	HookOnEvent HookType = "on_event"
)

const (
	minimumHookPriority = -100
	maximumHookPriority = 100
	maximumHookFailures = 256
)

// HookType identifies a point in the agent lifecycle.
type HookType string

// HookFunc transforms hook data or observes it without blocking the loop.
type HookFunc func(ctx context.Context, data any) (any, error)

// HookRegistry registers ordered lifecycle hooks.
type HookRegistry interface {
	Register(hookType HookType, priority int, hook HookFunc) error
	Unregister(hookType HookType, hook HookFunc) error
}

// HookFailure records an isolated hook failure without changing loop data.
type HookFailure struct {
	Type         HookType  `json:"type"`
	Priority     int       `json:"priority"`
	RegisteredAt time.Time `json:"registered_at"`
	PanicValue   string    `json:"panic_value,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// ErrInvalidHookPriority indicates a priority outside the inclusive -100 to 100 range.
var ErrInvalidHookPriority = errors.New("core: hook priority must be between -100 and 100")

// ErrNilHook indicates an attempt to register or remove a nil hook.
var ErrNilHook = errors.New("core: hook must not be nil")

// ErrInvalidHookType indicates an unknown hook type.
var ErrInvalidHookType = errors.New("core: invalid hook type")

// ErrHookNotRegistered indicates an unregister request with no matching hook.
var ErrHookNotRegistered = errors.New("core: hook is not registered")

type hookEntry struct {
	hook         HookFunc
	priority     int
	sequence     uint64
	registeredAt time.Time
	unsubscribe  func()
}

type hookRegistry struct {
	mu              sync.RWMutex
	bus             *asyncEventBus
	sessionID       string
	agentID         string
	entries         map[HookType][]*hookEntry
	nextSequence    uint64
	failures        []HookFailure
	droppedFailures uint64
}

func newHookRegistry(bus *asyncEventBus, sessionID string, agentID string) *hookRegistry {
	return &hookRegistry{
		bus:       bus,
		sessionID: sessionID,
		agentID:   agentID,
		entries:   make(map[HookType][]*hookEntry),
	}
}

func (r *hookRegistry) Register(hookType HookType, priority int, hook HookFunc) error {
	if !validHookType(hookType) {
		return ErrInvalidHookType
	}
	if priority < minimumHookPriority || priority > maximumHookPriority {
		return ErrInvalidHookPriority
	}
	if hook == nil {
		return ErrNilHook
	}

	r.mu.Lock()
	r.nextSequence++
	entry := &hookEntry{
		hook:         hook,
		priority:     priority,
		sequence:     r.nextSequence,
		registeredAt: time.Now().UTC(),
	}
	if hookType == HookOnEvent {
		entry.unsubscribe = r.bus.Subscribe(EventTypeAll, func(ctx context.Context, event Event) {
			_, regularErr, failure := invokeHook(
				ctx,
				hook,
				event,
				hookType,
				priority,
				entry.registeredAt,
			)
			if failure != nil {
				r.recordFailure(*failure)
				return
			}
			if regularErr != nil {
				r.recordFailure(HookFailure{
					Type:         hookType,
					Priority:     priority,
					RegisteredAt: entry.registeredAt,
					Error:        regularErr.Error(),
				})
			}
		})
	}
	r.entries[hookType] = append(r.entries[hookType], entry)
	sort.SliceStable(r.entries[hookType], func(left, right int) bool {
		if r.entries[hookType][left].priority == r.entries[hookType][right].priority {
			return r.entries[hookType][left].sequence < r.entries[hookType][right].sequence
		}
		return r.entries[hookType][left].priority < r.entries[hookType][right].priority
	})
	r.mu.Unlock()
	return nil
}

func (r *hookRegistry) Unregister(hookType HookType, hook HookFunc) error {
	if !validHookType(hookType) {
		return ErrInvalidHookType
	}
	if hook == nil {
		return ErrNilHook
	}

	hookPointer := reflect.ValueOf(hook).Pointer()
	r.mu.Lock()
	entries := r.entries[hookType]
	kept := entries[:0]
	unsubscribeFunctions := make([]func(), 0, len(entries))
	removed := false
	for _, entry := range entries {
		if reflect.ValueOf(entry.hook).Pointer() == hookPointer {
			removed = true
			if entry.unsubscribe != nil {
				unsubscribeFunctions = append(unsubscribeFunctions, entry.unsubscribe)
			}
			continue
		}
		kept = append(kept, entry)
	}
	if removed {
		if len(kept) == 0 {
			delete(r.entries, hookType)
		} else {
			r.entries[hookType] = kept
		}
	}
	r.mu.Unlock()

	for _, unsubscribe := range unsubscribeFunctions {
		unsubscribe()
	}
	if !removed {
		return ErrHookNotRegistered
	}
	return nil
}

func (r *hookRegistry) trigger(ctx context.Context, hookType HookType, data any) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.RLock()
	entries := append([]*hookEntry(nil), r.entries[hookType]...)
	r.mu.RUnlock()

	value := data
	for _, entry := range entries {
		result, err, failure := invokeHook(
			ctx,
			entry.hook,
			value,
			hookType,
			entry.priority,
			entry.registeredAt,
		)
		if failure != nil {
			r.recordFailure(*failure)
			r.publishFailure(*failure)
			continue
		}
		if err != nil {
			return value, fmt.Errorf("core: hook %s at priority %d: %w", hookType, entry.priority, err)
		}
		if result != nil {
			value = result
		}
	}
	return value, nil
}

func (r *hookRegistry) recordFailure(failure HookFailure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.failures) == maximumHookFailures {
		copy(r.failures, r.failures[1:])
		r.failures[len(r.failures)-1] = failure
		r.droppedFailures++
		return
	}
	r.failures = append(r.failures, failure)
}

func (r *hookRegistry) publishFailure(failure HookFailure) {
	if failure.Type == HookOnEvent {
		return
	}
	event, err := newEvent(EventTypeError, failure)
	if err != nil {
		return
	}
	event.Error = "hook panic isolated"
	event.SessionID = r.sessionID
	event.AgentID = r.agentID
	r.bus.Publish(context.Background(), event)
}

func invokeHook(
	ctx context.Context,
	hook HookFunc,
	data any,
	hookType HookType,
	priority int,
	registeredAt time.Time,
) (value any, err error, failure *HookFailure) {
	defer func() {
		if recovered := recover(); recovered != nil {
			failure = &HookFailure{
				Type:         hookType,
				Priority:     priority,
				RegisteredAt: registeredAt,
				PanicValue:   fmt.Sprint(recovered),
			}
		}
	}()
	value, err = hook(ctx, data)
	return value, err, nil
}

func validHookType(hookType HookType) bool {
	switch hookType {
	case HookPreLoop,
		HookPostLoop,
		HookPreModelCall,
		HookPostModelCall,
		HookToolCallReceived,
		HookPreToolExec,
		HookPostToolExec,
		HookToolResult,
		HookPreCompact,
		HookPostCompact,
		HookContextInject,
		HookSubAgentSpawn,
		HookSubAgentDone,
		HookOnEvent:
		return true
	default:
		return false
	}
}
