// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
)

// defaultShell prefers bash, because that is what a script written for a POSIX shell
// expects, and falls back through $SHELL to sh.
//
// The order is checked by looking rather than by asking the host what it has, because
// the host will happily report a shell for a user who has one installed but whose
// session cannot start it, and a shell that cannot be started is worse than one that
// was not chosen.
func defaultShell() ShellChoice {
	if _, err := os.Stat("/bin/bash"); err == nil {
		return ShellChoice{Path: "/bin/bash", Interactive: true}
	}
	if fromEnv := os.Getenv("SHELL"); fromEnv != "" {
		if _, err := exec.LookPath(fromEnv); err == nil {
			return ShellChoice{Path: fromEnv, Interactive: true}
		}
	}
	// sh is required to exist on a Unix host, so reaching here means something is
	// wrong with the machine rather than with the configuration. Saying which shell
	// was looked for is more use than "not found".
	return ShellChoice{
		Err: fmt.Errorf("platform: no shell found; looked for /bin/bash, $SHELL and sh"),
	}
}

// shellArgs runs a script with a login shell so that the profile the user expects is
// the one in effect, which is the difference between a command working and working
// only for someone whose profile happens to set the path.
func shellArgs(choice ShellChoice, script string) []string {
	return []string{choice.Path, "-lc", script}
}
