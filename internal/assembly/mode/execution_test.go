// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"context"
	"testing"
)

// The three reaches a coding turn can run at are the only names there are.
// Empty is build, which is what every conversation has been running at; a
// spelling this build does not know is an error rather than a guess, because a
// turn that ran at a level nobody asked for is a reach nobody can account for.
func TestNormalizeExecution(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    ExecutionMode
		wantErr bool
	}{
		{name: "empty is build", value: "", want: ExecBuild},
		{name: "build is build", value: "build", want: ExecBuild},
		{name: "plan is plan", value: "plan", want: ExecPlan},
		{name: "full is full", value: "full", want: ExecFull},
		{name: "padded empty is build", value: "  ", want: ExecBuild},
		{name: "an unknown name is refused", value: "sudo", wantErr: true},
		{name: "a case that is not the name is refused", value: "Full", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeExecution(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("NormalizeExecution(%q) = %q, want an error", test.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeExecution(%q) error = %v", test.value, err)
			}
			if got != test.want {
				t.Fatalf("NormalizeExecution(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

// Each level carries the tools its reach is: plan reads and searches, build
// and full access get the six working parts. The levels are built on the same
// assembly and reused, so a conversation that moves between them opens the
// shell once per level, not once per turn.
func TestExecutionLevelsCarryTheirReach(t *testing.T) {
	assembly, err := NewCoding(newTestProject(t), Parts{})
	if err != nil {
		t.Fatal(err)
	}
	namesOf := func(exec ExecutionMode) []string {
		t.Helper()
		toolSet, err := assembly.Tools(context.Background(), exec)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(toolSet))
		for _, tool := range toolSet {
			names = append(names, tool.Name())
		}
		return names
	}

	plan := namesOf(ExecPlan)
	if len(plan) != 3 {
		t.Fatalf("plan tools = %v, want the three read-only ones", plan)
	}
	for _, name := range plan {
		if name != "read" && name != "glob" && name != "grep" {
			t.Fatalf("plan carries %q, which can change the project", name)
		}
	}

	build, err := assembly.Tools(context.Background(), ExecBuild)
	if err != nil {
		t.Fatal(err)
	}
	if len(build) != 6 {
		t.Fatalf("build tools = %v, want the six", namesOf(ExecBuild))
	}
	full, err := assembly.Tools(context.Background(), ExecFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 6 {
		t.Fatalf("full tools = %v, want the same six ungated", namesOf(ExecFull))
	}

	// The build level hands out the same shell twice; the full level's shell is
	// a different shell, because it runs without the sandbox the build one is
	// behind.
	again, err := assembly.Tools(context.Background(), ExecBuild)
	if err != nil {
		t.Fatal(err)
	}
	if again[5] != build[5] {
		t.Fatal("the build shell was rebuilt instead of reused")
	}
	if build[5] == full[5] {
		t.Fatal("the full-access level reused the sandboxed shell")
	}
}
