// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"fmt"
	"strings"
)

// ExecutionMode is how a coding turn is allowed to reach the machine: the
// three reach levels a coding conversation can run at, chosen in the composer
// and carried by the turn that is sent with it.
//
// A conversation can move between them freely; the turn that is sent under a
// mode is the turn that runs under it, and the mode is recorded with the
// conversation when that turn is sent.
type ExecutionMode string

const (
	// ExecBuild is the ordinary coding reach: the file tools, the search tools
	// and the shell, gated by the workspace policy.
	ExecBuild ExecutionMode = "build"
	// ExecPlan is reading and search only. Files are not written, commands
	// are not run, and the gate refuses anything that would change the
	// project, which is what keeps a plan a plan.
	ExecPlan ExecutionMode = "plan"
	// ExecFull is the workspace with no gates at all: every assembled tool
	// runs, the shell runs without a sandbox, and nothing is refused for
	// being outside the project.
	ExecFull ExecutionMode = "full"
)

// NormalizeExecution resolves what a request names. Empty is build, which is
// the level a conversation has been running at so far; anything outside the
// three named values is an error rather than a guess.
func NormalizeExecution(value string) (ExecutionMode, error) {
	switch trimmed := strings.TrimSpace(value); ExecutionMode(trimmed) {
	case "":
		return ExecBuild, nil
	case ExecBuild, ExecPlan, ExecFull:
		return ExecutionMode(trimmed), nil
	default:
		return "", fmt.Errorf("mode: unknown execution mode %q", trimmed)
	}
}

// Valid says whether the mode is one of the three.
func (m ExecutionMode) Valid() bool {
	switch m {
	case ExecBuild, ExecPlan, ExecFull:
		return true
	default:
		return false
	}
}
