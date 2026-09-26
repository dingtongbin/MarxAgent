// SPDX-License-Identifier: Apache-2.0

package security

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowlistPolicyPermitsOnlyWhatItNames(t *testing.T) {
	policy := &AllowlistPolicy{Tools: []string{"read", "grep"}}
	if decision := policy.Evaluate(Call{ToolName: "read"}); !decision.Allowed {
		t.Fatalf("read was refused: %#v", decision)
	}
	decision := policy.Evaluate(Call{ToolName: "bash"})
	if decision.Allowed {
		t.Fatal("bash was permitted by a policy that does not name it")
	}
	if decision.Rule != "allowlist.tool" || decision.Reason == "" {
		t.Fatalf("decision = %#v", decision)
	}
	if policy.Name() != "allowlist" {
		t.Fatalf("name = %q", policy.Name())
	}
}

func TestAllowlistPolicyMatchesFamilies(t *testing.T) {
	policy := &AllowlistPolicy{Patterns: []string{"lsp_*", "wiki_*"}}
	for _, tool := range []string{"lsp_definition", "lsp_hover", "wiki_search"} {
		if decision := policy.Evaluate(Call{ToolName: tool}); !decision.Allowed {
			t.Fatalf("%s was refused: %#v", tool, decision)
		}
	}
	if policy.Evaluate(Call{ToolName: "lsp"}).Allowed {
		t.Fatal("a prefix matched as a glob")
	}
	// A pattern with no wildcard is an exact name, not a prefix.
	exact := &AllowlistPolicy{Patterns: []string{"lsp"}}
	if exact.Evaluate(Call{ToolName: "lsp_definition"}).Allowed {
		t.Fatal("a wildcard free pattern matched a prefix")
	}
	// A glob that does not compile as a match is treated as no match rather than
	// as a panic, because a policy comes from configuration. A name that is exactly
	// the pattern is still an exact match, which is the more useful reading.
	broken := &AllowlistPolicy{Patterns: []string{"[", "[a-", "x[0-9"}}
	for _, tool := range []string{"y", "z[", "[a-b"} {
		if decision := broken.Evaluate(Call{ToolName: tool}); decision.Allowed {
			t.Fatalf("%q was permitted by a broken pattern", tool)
		}
	}
}

func TestAllowlistDenyOverridesTheAllow(t *testing.T) {
	// A mode that permits a family and refuses one member of it is the case that
	// needs both lists, and the refusal has to win.
	policy := &AllowlistPolicy{Patterns: []string{"lsp_*"}, Denied: []string{"lsp_rename"}}
	if !policy.Evaluate(Call{ToolName: "lsp_hover"}).Allowed {
		t.Fatal("a permitted member of the family was refused")
	}
	decision := policy.Evaluate(Call{ToolName: "lsp_rename"})
	if decision.Allowed || decision.Rule != "allowlist.denied" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestAllowlistDelegatesToItsBase(t *testing.T) {
	inner := &WorkspacePolicy{Roots: []string{filepath.Join(t.TempDir(), "project")}}
	policy := &AllowlistPolicy{Tools: []string{"write"}, Base: inner}
	inside := Call{ToolName: "write", Path: filepath.Join(inner.Roots[0], "main.go")}
	if decision := policy.Evaluate(inside); !decision.Allowed {
		t.Fatalf("a write inside the workspace was refused: %#v", decision)
	}
	outside := Call{ToolName: "write", Path: filepath.Join(t.TempDir(), "elsewhere", "main.go")}
	if policy.Evaluate(outside).Allowed {
		t.Fatal("a write outside the workspace was permitted")
	}
	// A nil base is a bare permission, and the caller's tools are still filtered.
	plain := &AllowlistPolicy{Tools: []string{"read"}}
	if decision := plain.Evaluate(Call{ToolName: "read", Path: "/anywhere/at/all"}); !decision.Allowed {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestWorkspacePolicyConfinesPaths(t *testing.T) {
	root := t.TempDir()
	policy := &WorkspacePolicy{Roots: []string{root}}
	cases := map[string]bool{
		filepath.Join(root, "main.go"):                 true,
		filepath.Join(root, "sub", "deep", "file.txt"): true,
		root:                                  true,
		filepath.Join(root+"-old", "main.go"): false,
		filepath.Join(root, "..", "escaped"):  false,
		filepath.Join(filepath.Dir(root), "elsewhere.go"): false,
	}
	for path, allowed := range cases {
		decision := policy.Evaluate(Call{ToolName: "read", Path: path})
		if decision.Allowed != allowed {
			t.Fatalf("path %q allowed = %v, want %v (%#v)", path, decision.Allowed, allowed, decision)
		}
	}
	// A relative path is refused with a reason, because the tool resolves it against
	// a working directory this gate cannot see.
	decision := policy.Evaluate(Call{ToolName: "read", Path: "relative/path.go"})
	if decision.Allowed {
		t.Fatal("a relative path was permitted")
	}
	if !strings.Contains(decision.Reason, "relative") {
		t.Fatalf("reason = %q", decision.Reason)
	}
	// A path with no roots configured is not judged by the path rules at all,
	// because an empty list is a mode that did not configure them.
	unbounded := &WorkspacePolicy{}
	if !unbounded.Evaluate(Call{ToolName: "read", Path: "/anywhere"}).Allowed {
		t.Fatal("a policy with no roots refused a path")
	}
	// A mode can opt out explicitly, which is different from leaving it unset.
	opted := &WorkspacePolicy{Roots: []string{root}, AllowOutsideWorkspace: true}
	if !opted.Evaluate(Call{ToolName: "read", Path: "/anywhere"}).Allowed {
		t.Fatal("an explicit opt out did not take effect")
	}
}

func TestWorkspacePolicyJudgesCommands(t *testing.T) {
	policy := &WorkspacePolicy{
		Roots:             []string{t.TempDir()},
		AllowCommands:     []string{"go", "git"},
		DangerousCommands: []string{"rm", "format", "shutdown"},
	}
	if decision := policy.Evaluate(Call{ToolName: "bash", Command: "go test ./..."}); !decision.Allowed {
		t.Fatalf("an allowed command was refused: %#v", decision)
	}
	// A dangerous command is permitted and routed to a human. It is a third answer,
	// not a refusal, because the work mode does allow a shell.
	decision := policy.Evaluate(Call{ToolName: "bash", Command: "rm -rf /tmp/x"})
	if !decision.Allowed || !decision.RequiresApproval {
		t.Fatalf("decision = %#v", decision)
	}
	if !decision.Dangerous || decision.Rule != "workspace.dangerous" {
		t.Fatalf("decision = %#v", decision)
	}
	// A command that is on no list is refused.
	decision = policy.Evaluate(Call{ToolName: "bash", Command: "curl http://example.invalid"})
	if decision.Allowed || decision.Rule != "workspace.command" {
		t.Fatalf("decision = %#v", decision)
	}
	// A deny list that is empty means nothing is exempt, which is the safe reading
	// of a mode that did not configure one.
	strict := &WorkspacePolicy{Roots: []string{t.TempDir()}}
	if strict.Evaluate(Call{ToolName: "bash", Command: "go test"}).Allowed {
		t.Fatal("a command ran with no command list configured")
	}
	// A mode that asks a human before every command.
	asking := &WorkspacePolicy{Roots: []string{t.TempDir()}, RequireApproval: true}
	decision = asking.Evaluate(Call{ToolName: "bash", Command: "go test"})
	if !decision.Allowed || !decision.RequiresApproval {
		t.Fatalf("decision = %#v", decision)
	}
	// A call with no command is judged only by the path rules.
	if !policy.Evaluate(Call{ToolName: "read"}).Allowed {
		t.Fatal("a call with no command was refused")
	}
}

func TestFirstTokenSkipsEnvironmentAssignments(t *testing.T) {
	cases := map[string]string{
		"go test ./...":                 "go",
		"GOFLAGS=-mod=mod go test":      "go",
		"  rm -rf /":                    "rm",
		"":                              "",
		"   ":                           "",
		"A=1 B=2":                       "",
		"npx --yes create-app":          "npx",
		"/usr/local/bin/python3 script": "/usr/local/bin/python3",
	}
	for command, wanted := range cases {
		if got := firstToken(command); got != wanted {
			t.Fatalf("firstToken(%q) = %q, want %q", command, got, wanted)
		}
	}
	// The command word is lowercased so a list written in one case still matches.
	if firstToken("GO build") != "go" {
		t.Fatalf("firstToken = %q", firstToken("GO build"))
	}
}

func TestNoToolsRefusesEverything(t *testing.T) {
	policy := NoTools{}
	if policy.Name() != "none" {
		t.Fatalf("name = %q", policy.Name())
	}
	decision := policy.Evaluate(Call{ToolName: "read"})
	if decision.Allowed || decision.Rule != "none" {
		t.Fatalf("decision = %#v", decision)
	}
	if !strings.Contains(decision.Reason, "read") {
		t.Fatalf("the refusal does not name the tool: %q", decision.Reason)
	}
}

func TestChainTakesTheFirstRefusalAndTheStrongestApproval(t *testing.T) {
	// The second policy permits only bash and adds an approval to it, which is
	// the shape the work mode needs: a broad allowlist narrowed by a rule about
	// one family.
	allowing := &AllowlistPolicy{Tools: []string{"bash", "read"}}
	approving := &AllowlistPolicy{
		Tools: []string{"bash", "read"},
		Base:  &WorkspacePolicy{DangerousCommands: []string{"rm"}},
	}
	chain := NewChain(allowing, approving)
	if !strings.Contains(chain.Name(), "allowlist") {
		t.Fatalf("name = %q", chain.Name())
	}
	// A refusal from any policy wins.
	decision := chain.Evaluate(Call{ToolName: "write"})
	if decision.Allowed {
		t.Fatal("a chain permitted a tool neither policy allows")
	}
	// An approval raised by a later policy survives, because the whole point of
	// composing is permitting broadly and narrowing narrowly.
	decision = chain.Evaluate(Call{ToolName: "bash", Command: "rm -rf /"})
	if !decision.Allowed || !decision.RequiresApproval {
		t.Fatalf("decision = %#v", decision)
	}
	// A call nothing objects to is a plain permission.
	if decision := chain.Evaluate(Call{ToolName: "read"}); !decision.Allowed || decision.RequiresApproval {
		t.Fatalf("decision = %#v", decision)
	}
	// Nil policies are dropped rather than panicking, because a mode assembling
	// from configuration can leave one unset.
	if NewChain(nil).Name() != "" {
		t.Fatalf("name = %q", NewChain(nil).Name())
	}
	if NewChain(nil).Evaluate(Call{ToolName: "read"}).Allowed != true {
		t.Fatal("an empty chain refused a call")
	}
}

func TestChainHoldsAnApprovalUntilEveryPolicyAgrees(t *testing.T) {
	// A later policy can still refuse, so the approval must not be returned
	// before the whole chain has agreed the call is permitted.
	approving := &AllowlistPolicy{
		Tools: []string{"bash"},
		Base:  &WorkspacePolicy{DangerousCommands: []string{"rm"}},
	}
	refusing := &AllowlistPolicy{Tools: []string{"read"}}
	chain := NewChain(approving, refusing)
	decision := chain.Evaluate(Call{ToolName: "bash", Command: "rm -rf /"})
	if decision.Allowed {
		t.Fatalf("an approval survived a later refusal: %#v", decision)
	}
}

func TestRecorderCountsDecisions(t *testing.T) {
	recorder := NewRecorder(NewChain(
		&AllowlistPolicy{Tools: []string{"read", "bash"}},
		&WorkspacePolicy{Roots: []string{t.TempDir()}, DangerousCommands: []string{"rm"}},
	))
	if !strings.Contains(recorder.Name(), "recorder") {
		t.Fatalf("name = %q", recorder.Name())
	}
	recorder.Evaluate(Call{ToolName: "read"})
	recorder.Evaluate(Call{ToolName: "bash", Command: "rm -rf /"})
	recorder.Evaluate(Call{ToolName: "bash", Command: "curl http://example.invalid"})
	recorder.Evaluate(Call{ToolName: "write"})
	tally := recorder.Tally()
	if tally.Allowed != 1 {
		t.Fatalf("allowed = %d", tally.Allowed)
	}
	if tally.NeedsApproval != 1 {
		t.Fatalf("needs approval = %d", tally.NeedsApproval)
	}
	if tally.Denied != 2 {
		t.Fatalf("denied = %d", tally.Denied)
	}
	if tally.ByRule["allowlist.tool"] != 1 {
		t.Fatalf("by rule = %#v", tally.ByRule)
	}
	// The rule list is stable, so a report does not reshuffle between reads.
	first := recorder.Rules()
	for attempt := 0; attempt < 5; attempt++ {
		again := recorder.Rules()
		for index := range again {
			if again[index] != first[index] {
				t.Fatalf("the rule order changed: %v then %v", first, again)
			}
		}
	}
	// A recorder with no base records everything as allowed rather than panicking,
	// so wrapping an unset policy is survivable.
	bare := NewRecorder(nil)
	if bare.Name() != "recorder" {
		t.Fatalf("name = %q", bare.Name())
	}
	if !bare.Evaluate(Call{ToolName: "read"}).Allowed {
		t.Fatal("a recorder with no base refused a call")
	}
	if bare.Tally().Allowed != 1 {
		t.Fatal("a bare recorder did not count")
	}
}

func TestRecorderHandsOutACopyOfItsTally(t *testing.T) {
	recorder := NewRecorder(&AllowlistPolicy{Tools: []string{"read"}})
	recorder.Evaluate(Call{ToolName: "write"})
	tally := recorder.Tally()
	tally.ByRule["injected"] = 99
	if recorder.Tally().ByRule["injected"] != 0 {
		t.Fatal("the caller mutated the recorder's tally")
	}
}
