// SPDX-License-Identifier: Apache-2.0
//go:build windows

package platform

import (
	"fmt"
	"os/exec"
)

// defaultShell prefers PowerShell, and falls back to the older one.
//
// pwsh is preferred because it is the one that behaves the way a modern shell is
// expected to: it takes a command from a string the same way, and its output is
// UTF-8 rather than whatever the console code page happens to be. Falling back to
// Windows PowerShell is a real answer rather than a placeholder, because it is what
// ships with every supported Windows and will run the same script.
func defaultShell() ShellChoice {
	for _, name := range []string{"pwsh", "powershell"} {
		if found, err := exec.LookPath(name); err == nil {
			return ShellChoice{Path: found, Interactive: true}
		}
	}
	// Both are absent, which on a supported Windows means the PATH has been replaced
	// with something that has neither. Saying what was looked for is the difference
	// between a caller that can fix it and one that cannot.
	return ShellChoice{
		Err: fmt.Errorf("platform: no shell found; looked for pwsh and powershell"),
	}
}

// shellArgs runs a script with a shell that will not stop to ask anything.
//
// The three leading flags are the point. NoLogo keeps a banner out of the output a
// caller is about to parse, NoProfile keeps a user profile from changing what a
// command means partway through, and NonInteractive keeps a script that reads from
// standard input from hanging on a prompt that nobody is there to answer.
func shellArgs(choice ShellChoice, script string) []string {
	return []string{
		choice.Path, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script,
	}
}
