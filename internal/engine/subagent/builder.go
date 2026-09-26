// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// BuilderConfig configures the builder that makes a sub agent's own agent.
type BuilderConfig struct {
	// Provider reaches the model. It is required.
	Provider core.Provider
	// MainID is the main core, which is the parent every sub agent records.
	MainID string
	// Tools resolves a name to a tool. It is how a template's list of names becomes
	// the tools a sub agent may actually call, and it is required.
	Tools ToolResolver
	// Reserved names tools a sub agent may never be given, whatever a template asks
	// for. The orchestration tools go here.
	//
	// This is the whole reason a sub agent cannot spawn more of itself. A template is
	// a file, and a file is something a caller, a repository or a person can write,
	// so a template asking for the power to direct sub agents is a request that has to
	// be refused rather than obeyed. The names are matched exactly, because a prefix
	// match would let a template invent a name one edit away from a reserved one.
	Reserved []string
	// Defaults fills in what a template leaves out.
	Defaults BuilderDefaults
}

// BuilderDefaults are the settings a template does not choose for itself.
type BuilderDefaults struct {
	// Model is the model to run on when a template names none.
	Model string
	// MaxIterations bounds a sub agent's own loop.
	//
	// It has a default because a sub agent that keeps thinking is a sub agent that
	// spends the main core's budget without ever coming back, and a template that
	// forgot to say so should not get to spend it without limit.
	MaxIterations int
	// SystemSuffix is appended to every sub agent's instructions.
	SystemSuffix string
	// SessionPrefix names the session a sub agent's conversation is kept under.
	SessionPrefix string
}

// ToolResolver turns a name into a tool.
//
// It is an interface because which tools exist is the assembly layer's business, and
// this package's business is what a sub agent is allowed to ask for. Keeping the two
// apart is what lets the confinement rule below be enforced here at all.
type ToolResolver interface {
	// Lookup returns a tool by name.
	Lookup(name string) (core.Tool, bool)
}

// ResolverFunc adapts a function to a ToolResolver.
type ResolverFunc func(name string) (core.Tool, bool)

// Lookup calls the function.
func (f ResolverFunc) Lookup(name string) (core.Tool, bool) { return f(name) }

// CoreBuilder makes a sub agent's agent, and is what confines it.
//
// Every task gets a new agent, because a core agent holds its conversation and there
// is no way to clear one. Reusing it would put the previous task's transcript in front
// of a sub agent that was asked something else, and it would answer from it.
type CoreBuilder struct {
	config BuilderConfig
}

// NewCoreBuilder builds the builder.
func NewCoreBuilder(config BuilderConfig) (*CoreBuilder, error) {
	if config.Provider == nil {
		return nil, fmt.Errorf("subagent: the builder needs a provider")
	}
	if config.MainID == "" {
		return nil, fmt.Errorf("subagent: the builder needs the main core's identifier")
	}
	if config.Tools == nil {
		return nil, fmt.Errorf("subagent: the builder needs a way to resolve tools")
	}
	if config.Defaults.MaxIterations <= 0 {
		config.Defaults.MaxIterations = 20
	}
	if config.Defaults.SessionPrefix == "" {
		config.Defaults.SessionPrefix = "subagent"
	}
	return &CoreBuilder{config: config}, nil
}

// Build makes the agent for one task.
//
// The tool list is the template's, minus anything reserved, minus anything that does
// not exist. A name that cannot be resolved is dropped rather than passed through
// empty, because a request that declares a tool the caller cannot run would have the
// model call it and be refused, and a model that is refused a tool it was told it had
// wastes a turn and then works around it.
func (b *CoreBuilder) Build(ctx context.Context, template Template, slot *Slot) (core.Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if slot == nil {
		return nil, fmt.Errorf("subagent: the builder was given no slot")
	}
	reserved := b.reserved()
	allowed := make([]core.Tool, 0, len(template.Tools))
	var dropped []string
	for _, name := range template.Tools {
		if _, isReserved := reserved[name]; isReserved {
			dropped = append(dropped, name)
			continue
		}
		tool, ok := b.config.Tools.Lookup(name)
		if !ok || tool == nil {
			dropped = append(dropped, name)
			continue
		}
		allowed = append(allowed, tool)
	}
	// A stable order, because the tool list goes into a cached prompt and a set that
	// reordered itself between turns would throw that cache away for nothing.
	sort.Slice(allowed, func(i, j int) bool { return allowed[i].Name() < allowed[j].Name() })

	iterations := template.MaxIterations
	if iterations <= 0 {
		iterations = b.config.Defaults.MaxIterations
	}
	model := template.Model
	if model == "" {
		model = b.config.Defaults.Model
	}
	if model == "" {
		return nil, fmt.Errorf(
			"subagent: the template %q names no model and the builder has no default, so there is nothing to run",
			template.Name)
	}
	hint := strings.TrimSpace(template.Instructions + "\n\n" + b.config.Defaults.SystemSuffix)

	agent, err := core.NewAgent(b.config.Provider, core.Config{
		AgentID:       slot.ID,
		ParentID:      b.config.MainID,
		SessionID:     b.config.Defaults.SessionPrefix + ":" + slot.ID,
		Model:         model,
		SystemHint:    hint,
		MaxIterations: iterations,
		Tools:         allowed,
		Metadata: map[string]any{
			"template": template.Name,
			"slot":     slot.ID,
			"parent":   b.config.MainID,
			// A sub agent is one step from the top and cannot spawn more of itself,
			// so it is said here rather than left to be inferred from the tool list.
			"delegable": false,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("subagent: build the agent for %s: %w", slot.ID, err)
	}
	// What the template asked for and did not get is kept on the slot, where the main
	// core reads it. It is not written to the summary, which is the task's own account
	// of itself and would be overwritten by a note about the template.
	slot.mu.Lock()
	slot.refused = dropped
	slot.mu.Unlock()
	return agent, nil
}

// reserved is the set of names a sub agent may never be given.
func (b *CoreBuilder) reserved() map[string]struct{} {
	names := make(map[string]struct{}, len(b.config.Reserved))
	for _, name := range b.config.Reserved {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names[trimmed] = struct{}{}
		}
	}
	return names
}

// ReservedNames reports the names this builder refuses, so a caller can check its own
// configuration against them rather than finding out from a sub agent that came back
// without the tool it was promised.
func (b *CoreBuilder) ReservedNames() []string {
	reserved := b.reserved()
	names := make([]string, 0, len(reserved))
	for name := range reserved {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
