// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// jsonMarshal is a variable so a test can see what the pipeline journaled.
var jsonMarshal = json.Marshal

// ErrOrchestration reports that a composition could not finish.
var ErrOrchestration = errors.New("subagent: the composition could not finish")

// Result is one agent's outcome.
type Result struct {
	// AgentID is who produced it.
	AgentID string
	// Name is the agent's label.
	Name string
	// Err is why it failed, nil on success.
	Err error
	// Output is whatever the agent returned.
	Output any
}

// Succeeded reports whether this result is a success.
func (r Result) Succeeded() bool { return r.Err == nil }

// Orchestration runs several agents over one task and collects their outcomes.
//
// The patterns are here as functions rather than as a framework because they are
// three shapes, not a spectrum, and a caller composing them by hand is more able
// to see what the assembly will actually do. Scheduling a trigger is the
// assembly layer's job and is deliberately absent.
type Orchestration struct {
	// Registry creates the agents.
	Registry *Registry
}

// NewOrchestration builds an orchestrator.
func NewOrchestration(registry *Registry) (*Orchestration, error) {
	if registry == nil {
		return nil, fmt.Errorf("subagent: an orchestration needs a registry")
	}
	return &Orchestration{Registry: registry}, nil
}

// Sequential runs the agents one after another and stops at the first failure.
//
// Stopping is the point: a later stage that depends on an earlier one's output
// would be working from nothing, and running it anyway would produce a result
// that looks complete.
func (o *Orchestration) Sequential(
	ctx context.Context, specs []Spec, run func(ctx context.Context, agentID string) (any, error),
) ([]Result, error) {
	if run == nil {
		return nil, fmt.Errorf("subagent: a composition needs something to run")
	}
	results := make([]Result, 0, len(specs))
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		agent, err := o.Registry.Spawn(ctx, o.Registry.MainID(), spec)
		if err != nil {
			return results, fmt.Errorf("%w: %v", ErrOrchestration, err)
		}
		output, runErr := o.runOne(ctx, agent, run)
		results = append(results, Result{
			AgentID: agent.ID, Name: agent.Name, Err: runErr, Output: output,
		})
		if runErr != nil {
			return results, fmt.Errorf("%w: %q failed: %w", ErrOrchestration, spec.Name, runErr)
		}
	}
	return results, nil
}

// Parallel runs every agent at once and waits for all of them.
//
// One failure does not cancel the others. A fan out is usually a set of
// independent questions, and cancelling the rest because one had no answer would
// throw away work that had already been paid for.
func (o *Orchestration) Parallel(
	ctx context.Context, specs []Spec, run func(ctx context.Context, agentID string) (any, error),
) ([]Result, error) {
	if run == nil {
		return nil, fmt.Errorf("subagent: a composition needs something to run")
	}
	if len(specs) == 0 {
		return nil, nil
	}
	agents := make([]Agent, 0, len(specs))
	for _, spec := range specs {
		agent, err := o.Registry.Spawn(ctx, o.Registry.MainID(), spec)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrOrchestration, err)
		}
		agents = append(agents, agent)
	}

	results := make([]Result, len(agents))
	var wg sync.WaitGroup
	for index, agent := range agents {
		wg.Add(1)
		go func(position int, current Agent) {
			defer wg.Done()
			output, runErr := o.runOne(ctx, current, run)
			results[position] = Result{
				AgentID: current.ID, Name: current.Name, Err: runErr, Output: output,
			}
		}(index, agent)
	}
	wg.Wait()

	failures := 0
	var firstErr error
	for _, result := range results {
		if result.Succeeded() {
			continue
		}
		failures++
		if firstErr == nil {
			firstErr = result.Err
		}
	}
	if failures > 0 {
		// The results are returned alongside the error, because a caller usually
		// wants the ones that worked as much as the one that did not.
		return results, fmt.Errorf("%w: %d of %d agents failed, first was %w",
			ErrOrchestration, failures, len(agents), firstErr)
	}
	return results, nil
}

// Pipeline threads each agent's output into the next one's input.
//
// The stages run one after another because that is what a pipeline is for: each
// stage transforms what the previous one produced, and running them at once would
// leave every stage with nothing to do.
func (o *Orchestration) Pipeline(
	ctx context.Context, specs []Spec, run func(ctx context.Context, agentID string, input any) (any, error),
) ([]Result, error) {
	if run == nil {
		return nil, fmt.Errorf("subagent: a composition needs something to run")
	}
	if len(specs) == 0 {
		return nil, nil
	}
	results := make([]Result, 0, len(specs))
	var carried any
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		agent, err := o.Registry.Spawn(ctx, o.Registry.MainID(), spec)
		if err != nil {
			return results, fmt.Errorf("%w: %v", ErrOrchestration, err)
		}
		output, runErr := o.runPipelineStage(ctx, agent, carried, run)
		results = append(results, Result{
			AgentID: agent.ID, Name: agent.Name, Err: runErr, Output: output,
		})
		if runErr != nil {
			return results, fmt.Errorf("%w: stage %q failed: %w", ErrOrchestration, spec.Name, runErr)
		}
		carried = output
	}
	return results, nil
}

// runOne drives one agent and records its outcome, so a caller can see what
// happened even when the composition reports a failure.
func (o *Orchestration) runOne(
	ctx context.Context, agent Agent, run func(ctx context.Context, agentID string) (any, error),
) (any, error) {
	if err := o.Registry.SetState(agent.ID, StateRunning); err != nil {
		return nil, err
	}
	if o.Registry.Broker() != nil {
		if _, err := o.Registry.Broker().Send(ctx, Message{
			FromAgentID: o.Registry.MainID(),
			ToAgentID:   agent.ID,
			Type:        TypeTask,
			Content:     agent.Name,
			Timestamp:   timeNow(),
		}); err != nil {
			// A task that was never promised must not be run, because the journal
			// is what makes a sub agent's work auditable.
			_ = o.Registry.Finish(agent.ID, err)
			return nil, err
		}
	}
	output, err := run(ctx, agent.ID)
	if finishErr := o.Registry.Finish(agent.ID, err); finishErr != nil {
		return output, finishErr
	}
	summary := fmt.Sprint(output)
	if len(summary) > 200 {
		summary = summary[:200]
	}
	_ = o.Registry.SetSummary(agent.ID, summary)
	return output, err
}

func (o *Orchestration) runPipelineStage(
	ctx context.Context, agent Agent, input any,
	run func(ctx context.Context, agentID string, input any) (any, error),
) (any, error) {
	if err := o.Registry.SetState(agent.ID, StateRunning); err != nil {
		return nil, err
	}
	if o.Registry.Broker() != nil {
		if _, err := o.Registry.Broker().Send(ctx, Message{
			FromAgentID: o.Registry.MainID(),
			ToAgentID:   agent.ID,
			Type:        TypeTask,
			Content:     agent.Name,
			Data:        encodeInput(input),
			Timestamp:   timeNow(),
		}); err != nil {
			_ = o.Registry.Finish(agent.ID, err)
			return nil, err
		}
	}
	output, err := run(ctx, agent.ID, input)
	if finishErr := o.Registry.Finish(agent.ID, err); finishErr != nil {
		return output, finishErr
	}
	_ = o.Registry.SetSummary(agent.ID, fmt.Sprint(output))
	return output, err
}

// encodeInput renders a pipeline stage's input for the journal. An input that will
// not encode is dropped rather than failing the stage, because the stage can still
// run: the input is a convenience for the audit trail, not the delivery mechanism.
func encodeInput(input any) []byte {
	if input == nil {
		return nil
	}
	encoded, err := jsonMarshal(input)
	if err != nil {
		return nil
	}
	return encoded
}
