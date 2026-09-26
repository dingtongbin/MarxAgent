// SPDX-License-Identifier: Apache-2.0

package security

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Decision is what a gate concluded about one tool call.
type Decision struct {
	// Allowed is whether the call may run.
	Allowed bool `json:"allowed"`
	// Reason explains a refusal, because a gate that refuses silently is
	// indistinguishable from a broken one.
	Reason string `json:"reason,omitempty"`
	// Rule names the rule that decided, so a refusal is traceable to policy rather
	// than to a version of the binary.
	Rule string `json:"rule,omitempty"`
	// RequiresApproval marks a call that is permitted but needs a human. This is a
	// third answer, not a flavour of refusal: the work mode allows a shell and
	// still requires a human for the dangerous ones.
	RequiresApproval bool `json:"requires_approval,omitempty"`
	// Dangerous marks a call the caller should surface to the user.
	Dangerous bool `json:"dangerous,omitempty"`
}

// Allow returns an unconditional permission.
func Allow() Decision { return Decision{Allowed: true} }

// Deny returns a refusal.
func Deny(rule, reason string) Decision {
	return Decision{Allowed: false, Rule: rule, Reason: reason}
}

// NeedsApproval returns a permission that is conditional on a human.
func NeedsApproval(rule, reason string) Decision {
	return Decision{Allowed: true, RequiresApproval: true, Rule: rule, Reason: reason, Dangerous: true}
}

// Call is the part of a tool call a gate needs. It is a small struct rather than
// the caller's own type so the gate does not drag a transport shape into policy.
type Call struct {
	// ToolName is the tool being called.
	ToolName string
	// Parameters is the raw json, because a path is usually buried in it and the
	// gate cannot know a caller's parameter struct.
	Parameters string
	// Path is a filesystem path the call touches, when the tool has one. It must
	// be absolute: a relative path is resolved by the tool against its own working
	// directory, which a gate cannot check, so the gate refuses it rather than
	// guessing. An empty path means the call has no filesystem reach.
	Path string
	// Command is a shell command the call runs, when it has one.
	Command string
}

// Policy gates tool calls.
//
// It is an interface because the modes differ completely: chat mode refuses every
// call that touches the filesystem or a shell, work mode allows them inside a
// project directory, and a mode with a web approval queue allows them and asks a
// human first. Those are assembled from the primitives below, never written out
// three times.
type Policy interface {
	// Evaluate decides one call.
	Evaluate(call Call) Decision
	// Name identifies the policy in a log line.
	Name() string
}

// AllowlistPolicy permits only the tools it names, optionally with patterns.
type AllowlistPolicy struct {
	// Tools are exact tool names.
	Tools []string
	// Patterns are glob patterns, for families such as every lsp_ tool. A pattern
	// is deliberately coarse on purpose: it is the caller's choice to be broad.
	Patterns []string
	// Base is the policy delegated to when the tool is permitted. A nil base means
	// a bare permission, and a nil base is the safer of the two mistakes to make
	// loudly rather than quietly.
	Base Policy
	// Denied wins over everything, so a mode can permit a family and still refuse
	// one member of it.
	Denied []string
}

// Name identifies the policy.
func (p *AllowlistPolicy) Name() string { return "allowlist" }

// Evaluate decides one call.
func (p *AllowlistPolicy) Evaluate(call Call) Decision {
	if len(p.Denied) > 0 {
		for _, denied := range p.Denied {
			if matchesTool(denied, call.ToolName) {
				return Deny("allowlist.denied",
					fmt.Sprintf("the tool %q is on the denied list of this mode", call.ToolName))
			}
		}
	}
	permitted := false
	for _, tool := range p.Tools {
		if tool == call.ToolName {
			permitted = true
			break
		}
	}
	if !permitted {
		for _, pattern := range p.Patterns {
			if matchesTool(pattern, call.ToolName) {
				permitted = true
				break
			}
		}
	}
	if !permitted {
		return Deny("allowlist.tool",
			fmt.Sprintf("the tool %q is not available in this mode", call.ToolName))
	}
	if p.Base != nil {
		return p.Base.Evaluate(call)
	}
	return Allow()
}

// matchesTool reports whether a name matches a pattern, treating a pattern with
// no wildcard as an exact match.
func matchesTool(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	matched, err := filepath.Match(pattern, name)
	return err == nil && matched
}

// WorkspacePolicy confines filesystem and command reach to a set of roots.
type WorkspacePolicy struct {
	// Roots are the directories a call may touch. A call with no path is judged
	// only by the rest of the policy.
	Roots []string
	// AllowCommands lists the commands that need no human. An empty list means no
	// command is exempt, which is the safe reading of a mode that did not say.
	AllowCommands []string
	// DangerousCommands need a human even though they are inside the workspace.
	DangerousCommands []string
	// AllowOutsideWorkspace permits a call whose path is outside every root. It
	// exists so a mode can opt in explicitly rather than by leaving a root empty.
	AllowOutsideWorkspace bool
	// RequireApproval marks every command as needing a human.
	RequireApproval bool
}

// Name identifies the policy.
func (p *WorkspacePolicy) Name() string { return "workspace" }

// Evaluate decides one call.
func (p *WorkspacePolicy) Evaluate(call Call) Decision {
	if call.Path != "" && len(p.Roots) > 0 && !p.AllowOutsideWorkspace {
		inside, err := withinRoots(p.Roots, call.Path)
		if err != nil {
			return Deny("workspace.path", err.Error())
		}
		if !inside {
			return Deny("workspace.path",
				fmt.Sprintf("the path %q is outside the workspace this mode is confined to", call.Path))
		}
	}
	if call.Command == "" {
		return Allow()
	}
	command := firstToken(call.Command)
	for _, dangerous := range p.DangerousCommands {
		if matchesTool(dangerous, command) {
			return NeedsApproval("workspace.dangerous",
				fmt.Sprintf("%q can change the machine or lose data, so it needs a human", command))
		}
	}
	if p.RequireApproval {
		return NeedsApproval("workspace.approval",
			"this mode asks a human before running any command")
	}
	for _, allowed := range p.AllowCommands {
		if matchesTool(allowed, command) {
			return Allow()
		}
	}
	return Deny("workspace.command",
		fmt.Sprintf("%q is not on the command list this mode permits", command))
}

// withinRoots reports whether a path lies inside one of the roots, comparing
// cleaned absolute paths and refusing a path that merely starts with the same
// characters, which is how a sibling directory named data-old passes a prefix
// test.
//
// A relative path is refused rather than resolved. The tool resolves it against
// its own working directory, which this gate cannot see, and resolving it against
// the process working directory instead would check the path against a root it
// has nothing to do with. Saying so is better than guessing.
func withinRoots(roots []string, path string) (bool, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return false, fmt.Errorf(
			"the path %q is relative, so it cannot be checked against the workspace roots", path)
	}
	for _, root := range roots {
		cleanRoot := filepath.Clean(root)
		if cleaned == cleanRoot {
			return true, nil
		}
		prefix := cleanRoot
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if strings.HasPrefix(cleaned, prefix) {
			return true, nil
		}
	}
	return false, nil
}

// firstToken returns the command word of a shell line, which is what a command
// list is written against.
func firstToken(command string) string {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 0 {
		return ""
	}
	// A leading environment assignment is not the command, so it is skipped
	// rather than treated as an unknown command and refused.
	index := 0
	for index < len(fields) && strings.Contains(fields[index], "=") &&
		!strings.HasPrefix(fields[index], "=") {
		index++
	}
	if index >= len(fields) {
		return ""
	}
	return strings.ToLower(fields[index])
}

// NoTools refuses every call, which is what a mode with no tools at all uses
// rather than a nil policy that a caller might skip.
type NoTools struct{}

// Name identifies the policy.
func (NoTools) Name() string { return "none" }

// Evaluate refuses every call.
func (NoTools) Evaluate(call Call) Decision {
	return Deny("none", fmt.Sprintf("this mode has no tools, so %q cannot run", call.ToolName))
}

// Chain runs policies in order and takes the first answer that is not a plain
// permission.
//
// A later policy can still add an approval requirement to a call an earlier one
// permitted, which is what lets a mode compose a broad allowlist with a narrow
// workspace rule instead of having to merge them by hand.
type Chain struct {
	policies []Policy
}

// NewChain composes policies.
func NewChain(policies ...Policy) *Chain {
	kept := make([]Policy, 0, len(policies))
	for _, policy := range policies {
		if policy != nil {
			kept = append(kept, policy)
		}
	}
	return &Chain{policies: kept}
}

// Name lists the composed policies.
func (c *Chain) Name() string {
	names := make([]string, 0, len(c.policies))
	for _, policy := range c.policies {
		names = append(names, policy.Name())
	}
	return strings.Join(names, "+")
}

// Evaluate decides one call.
func (c *Chain) Evaluate(call Call) Decision {
	decision := Allow()
	for _, policy := range c.policies {
		current := policy.Evaluate(call)
		if !current.Allowed {
			return current
		}
		if current.RequiresApproval || current.Dangerous {
			// A later policy may still refuse, so the approval is held back until
			// every policy has agreed the call is permitted at all.
			decision = current
		}
	}
	return decision
}

// Recorder counts decisions, so a mode can surface what its gates are doing
// without instrumenting each policy.
type Recorder struct {
	mu       sync.Mutex
	base     Policy
	allowed  int
	denied   int
	approval int
	byRule   map[string]int
}

// NewRecorder wraps a policy.
func NewRecorder(base Policy) *Recorder {
	return &Recorder{base: base, byRule: map[string]int{}}
}

// Name identifies the policy.
func (r *Recorder) Name() string {
	if r.base == nil {
		return "recorder"
	}
	return "recorder(" + r.base.Name() + ")"
}

// Evaluate decides one call and records the outcome.
func (r *Recorder) Evaluate(call Call) Decision {
	decision := Allow()
	if r.base != nil {
		decision = r.base.Evaluate(call)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !decision.Allowed:
		r.denied++
	case decision.RequiresApproval:
		r.approval++
	default:
		r.allowed++
	}
	if decision.Rule != "" {
		r.byRule[decision.Rule]++
	}
	return decision
}

// Tally summarizes the recorded decisions.
type Tally struct {
	Allowed       int            `json:"allowed"`
	Denied        int            `json:"denied"`
	NeedsApproval int            `json:"needs_approval"`
	ByRule        map[string]int `json:"by_rule"`
}

// Tally summarizes the recorder.
func (r *Recorder) Tally() Tally {
	r.mu.Lock()
	defer r.mu.Unlock()
	report := Tally{
		Allowed:       r.allowed,
		Denied:        r.denied,
		NeedsApproval: r.approval,
		ByRule:        make(map[string]int, len(r.byRule)),
	}
	for rule, count := range r.byRule {
		report.ByRule[rule] = count
	}
	return report
}

// Rules lists the rules that fired, in a stable order so a report does not
// reshuffle between reads.
func (r *Recorder) Rules() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.byRule))
	for name := range r.byRule {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
