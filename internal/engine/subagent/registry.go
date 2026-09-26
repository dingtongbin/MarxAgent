// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// State is where an agent is in its life.
type State string

const (
	// StateStarting means the agent exists but has not produced anything.
	StateStarting State = "starting"
	// StateRunning means the agent is working.
	StateRunning State = "running"
	// StateDone means the agent finished.
	StateDone State = "done"
	// StateFailed means the agent stopped on an error.
	StateFailed State = "failed"
)

// Spawner creates an agent. It is an interface so the infra does not depend on how
// an agent is built, which is the assembly layer's decision.
type Spawner interface {
	// Spawn starts an agent and returns a handle to it.
	Spawn(ctx context.Context, spec Spec) (Handle, error)
}

// Handle is a running agent.
type Handle interface {
	// AgentID reports the identifier the agent was given.
	AgentID() string
	// Run drives the agent to completion.
	Run(ctx context.Context) error
	// State reports where the agent is.
	State() State
}

// Spec describes an agent to create.
type Spec struct {
	// Name is a human readable label, used in logs and in the parent's own output.
	Name string
	// SystemPrompt is the agent's instructions. It is a separate prompt from the
	// parent's on purpose: a sub agent with its parent's prompt would inherit
	// permissions nobody meant to hand it.
	SystemPrompt string
	// ParentID is who is creating the agent.
	ParentID string
	// AgentID is the identifier to assign. An empty one is generated.
	AgentID string
	// Tools are the tools the agent may use. A sub agent is confined to what it was
	// given, so a parent's wider pool is not passed down by default.
	Tools []string
	// Metadata is carried for the caller.
	Metadata map[string]any
}

// Agent is one registered agent.
type Agent struct {
	// ID is the agent's identifier.
	ID string `json:"id"`
	// Name is its label.
	Name string `json:"name"`
	// ParentID is its creator, empty for the main core.
	ParentID string `json:"parent_id,omitempty"`
	// State is where it is in its life.
	State State `json:"state"`
	// Tools are the tools it may use.
	Tools []string `json:"tools,omitempty"`
	// Summary is a short description of what it was asked and what it produced,
	// which is what another agent sees when it asks about this one.
	Summary string `json:"summary,omitempty"`
	// MessageCount is how many messages it has exchanged, so a parent can tell a
	// busy agent from an idle one without reading its transcript.
	MessageCount int64 `json:"message_count"`
	// CreatedAt and UpdatedAt bound its life.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Err is why it failed, empty otherwise.
	Err string `json:"error,omitempty"`
}

// IsMain reports whether an agent is the main core. The design allows one level of
// sub agent, so an agent with a parent is at most one step from the top and a
// caller never has to walk a chain to know it.
func (a Agent) IsMain() bool { return a.ParentID == "" }

// timeNow is a variable so a test can make timestamps deterministic.
var timeNow = func() time.Time { return time.Now().UTC() }

// Registry owns the agents and the rule about who may create them.
type Registry struct {
	spawner Spawner
	broker  *Broker

	mu      sync.RWMutex
	agents  map[string]Agent
	handles map[string]Handle
	// mainID is the one identifier allowed to create agents.
	mainID string
	// maxAgents bounds how many can exist, so a runaway parent cannot exhaust the
	// machine. Zero means unbounded.
	maxAgents int
}

// RegistryConfig configures a registry.
type RegistryConfig struct {
	// Spawner creates agents. A nil spawner makes Spawn fail, which keeps the rule
	// testable without building a whole agent.
	Spawner Spawner
	// Broker carries the messages. A nil broker is allowed, because a mode may
	// have no sub agents at all.
	Broker *Broker
	// MainID is the main core's identifier.
	MainID string
	// MaxAgents bounds the population. Zero means unbounded.
	MaxAgents int
}

// NewRegistry builds a registry.
func NewRegistry(config RegistryConfig) (*Registry, error) {
	if config.MainID == "" {
		return nil, fmt.Errorf("subagent: a registry needs the main core identifier")
	}
	registry := &Registry{
		spawner:   config.Spawner,
		broker:    config.Broker,
		agents:    map[string]Agent{},
		handles:   map[string]Handle{},
		mainID:    config.MainID,
		maxAgents: config.MaxAgents,
	}
	registry.agents[config.MainID] = Agent{
		ID:        config.MainID,
		Name:      "main",
		State:     StateRunning,
		CreatedAt: timeNow(),
		UpdatedAt: timeNow(),
	}
	return registry, nil
}

// MainID reports the main core's identifier.
func (r *Registry) MainID() string { return r.mainID }

// Broker reports the message broker, which is nil for a mode with no sub agents.
func (r *Registry) Broker() *Broker { return r.broker }

// Spawn creates an agent.
//
// The rule is enforced here: only the main core may create, and a sub agent may
// not create another. A chain would multiply work no one bounded, and an audit
// trail no one could follow.
func (r *Registry) Spawn(ctx context.Context, parentID string, spec Spec) (Agent, error) {
	if parentID != r.mainID {
		return Agent{}, fmt.Errorf("%w: %q tried to create %q", ErrNotMain, parentID, spec.Name)
	}
	if r.spawner == nil {
		return Agent{}, fmt.Errorf("subagent: no spawner is configured, so no agent can be created")
	}
	if spec.AgentID == "" {
		spec.AgentID = fmt.Sprintf("sub-%d", r.nextIndex())
	}
	spec.ParentID = parentID

	r.mu.Lock()
	if r.maxAgents > 0 && len(r.agents) >= r.maxAgents {
		r.mu.Unlock()
		return Agent{}, fmt.Errorf("subagent: the registry already holds its maximum of %d agents", r.maxAgents)
	}
	if _, exists := r.agents[spec.AgentID]; exists {
		r.mu.Unlock()
		return Agent{}, fmt.Errorf("subagent: the agent %q already exists", spec.AgentID)
	}
	r.mu.Unlock()

	if r.broker != nil {
		if err := r.broker.Register(spec.AgentID); err != nil {
			return Agent{}, err
		}
	}
	handle, err := r.spawner.Spawn(ctx, spec)
	if err != nil {
		if r.broker != nil {
			r.broker.Unregister(spec.AgentID)
		}
		return Agent{}, fmt.Errorf("subagent: create %q: %w", spec.Name, err)
	}
	agent := Agent{
		ID:        handle.AgentID(),
		Name:      spec.Name,
		ParentID:  parentID,
		State:     handle.State(),
		Tools:     append([]string(nil), spec.Tools...),
		CreatedAt: timeNow(),
		UpdatedAt: timeNow(),
	}
	if agent.ID == "" {
		agent.ID = spec.AgentID
	}
	r.mu.Lock()
	r.agents[agent.ID] = agent
	r.handles[agent.ID] = handle
	r.mu.Unlock()
	return agent, nil
}

func (r *Registry) nextIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.agents)
}

// Get returns an agent.
func (r *Registry) Get(agentID string) (Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	agent, exists := r.agents[agentID]
	return agent, exists
}

// List returns every agent, the main core included.
func (r *Registry) List() []Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	agents := make([]Agent, 0, len(r.agents))
	for _, agent := range r.agents {
		agents = append(agents, agent)
	}
	return agents
}

// Delete removes an agent, which only the main core may do.
//
// Deleting is not the same as finishing. A finished agent is done and its record is
// what a later caller reads to find out what it did; a deleted one is gone from the
// registry, and that is only for a slot the main core has finished with.
func (r *Registry) Delete(ctx context.Context, byAgentID, agentID string) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if byAgentID != r.mainID {
		return fmt.Errorf("%w: %q tried to delete %q", ErrNotMain, byAgentID, agentID)
	}
	if agentID == r.mainID {
		// The main core is not a sub agent and cannot be deleted by this path, because
		// a registry with no main core has no way to enforce the rule it exists for.
		return fmt.Errorf("%w: the main core cannot be deleted", ErrNotMain)
	}
	r.mu.Lock()
	if _, exists := r.agents[agentID]; !exists {
		r.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrNoSuchAgent, agentID)
	}
	delete(r.agents, agentID)
	delete(r.handles, agentID)
	r.mu.Unlock()
	if r.broker != nil {
		r.broker.Unregister(agentID)
	}
	return nil
}

// Register records a place a sub agent can run.
//
// This is not Spawn. A slot exists before anything runs on it and is reused after
// each task, so it is registered once and lives until it is deleted, while Spawn is
// for an agent that has been built. Conflating them would mean a pool had to build
// an agent to hold a place, which is the thing this design exists to avoid.
func (r *Registry) Register(ctx context.Context, byAgentID string, spec Spec) (Agent, error) {
	if ctx == nil {
		return Agent{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return Agent{}, err
	}
	if byAgentID != r.mainID {
		return Agent{}, fmt.Errorf("%w: %q tried to register %q", ErrNotMain, byAgentID, spec.Name)
	}
	agentID := strings.TrimSpace(spec.AgentID)
	if agentID == "" {
		agentID = fmt.Sprintf("sub-%d", len(r.agents))
	}
	spec.AgentID = agentID
	spec.ParentID = byAgentID

	r.mu.Lock()
	if _, exists := r.agents[agentID]; exists {
		r.mu.Unlock()
		return Agent{}, fmt.Errorf("subagent: the agent %q already exists", agentID)
	}
	if r.maxAgents > 0 && len(r.agents) >= r.maxAgents {
		r.mu.Unlock()
		return Agent{}, fmt.Errorf("subagent: the registry already holds its maximum of %d agents", r.maxAgents)
	}
	agent := Agent{
		ID:        agentID,
		Name:      spec.Name,
		ParentID:  byAgentID,
		State:     StateStarting,
		Tools:     append([]string(nil), spec.Tools...),
		CreatedAt: timeNow(),
		UpdatedAt: timeNow(),
	}
	r.agents[agentID] = agent
	r.mu.Unlock()
	if r.broker != nil {
		if err := r.broker.Register(agentID); err != nil {
			r.mu.Lock()
			delete(r.agents, agentID)
			r.mu.Unlock()
			r.broker.Unregister(agentID)
			return Agent{}, err
		}
	}
	return agent, nil
}

// SetState records an agent's state.
func (r *Registry) SetState(agentID string, state State) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, exists := r.agents[agentID]
	if !exists {
		return fmt.Errorf("%w: %q", ErrNoSuchAgent, agentID)
	}
	agent.State = state
	agent.UpdatedAt = timeNow()
	r.agents[agentID] = agent
	if state == StateDone || state == StateFailed {
		if handle, held := r.handles[agentID]; held {
			delete(r.handles, agentID)
			_ = handle
		}
	}
	return nil
}

// SetSummary records what an agent was asked and produced, which is what another
// agent sees when it asks about this one.
func (r *Registry) SetSummary(agentID, summary string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, exists := r.agents[agentID]
	if !exists {
		return fmt.Errorf("%w: %q", ErrNoSuchAgent, agentID)
	}
	agent.Summary = summary
	agent.UpdatedAt = timeNow()
	r.agents[agentID] = agent
	return nil
}

// NoteMessage counts traffic to an agent, so a parent can tell a busy agent from
// an idle one without reading its transcript.
func (r *Registry) NoteMessage(agentID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	agent, exists := r.agents[agentID]
	if !exists {
		return fmt.Errorf("%w: %q", ErrNoSuchAgent, agentID)
	}
	agent.MessageCount++
	agent.UpdatedAt = timeNow()
	r.agents[agentID] = agent
	return nil
}

// Handle returns a running agent's handle.
func (r *Registry) Handle(agentID string) (Handle, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	handle, exists := r.handles[agentID]
	return handle, exists
}

// Finish records that an agent stopped, and says why when it stopped badly.
func (r *Registry) Finish(agentID string, runErr error) error {
	state := StateDone
	if runErr != nil {
		state = StateFailed
	}
	if err := r.SetState(agentID, state); err != nil {
		return err
	}
	if runErr != nil {
		r.mu.Lock()
		agent := r.agents[agentID]
		agent.Err = runErr.Error()
		r.agents[agentID] = agent
		r.mu.Unlock()
	}
	if r.broker != nil {
		r.broker.Unregister(agentID)
	}
	return nil
}

// Shutdown closes the broker, which stops every mailbox.
func (r *Registry) Shutdown() {
	if r.broker != nil {
		r.broker.Close()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handles = map[string]Handle{}
}
