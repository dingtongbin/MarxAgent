// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"errors"
	"fmt"
)

// ShellChoice is a command to run a script with, and the flags that go with it.
type ShellChoice struct {
	// Path is the executable to run. Empty means none could be found on this host.
	Path string
	// Interactive reports whether the shell can run a command from a string, as
	// opposed to only running a file. A host whose shell cannot do that needs the
	// script written to a file first, and a caller that assumed otherwise would hand
	// a command to something that cannot take one.
	Interactive bool
	// Err says why no shell was found, when none was.
	Err error
}

// Args builds the argument list that runs a script with this shell.
//
// It is here rather than in the tool because knowing how to start a shell is a fact
// about the host, and the argument assembly that goes with it is the half of the
// answer a tool cannot avoid. Splitting them this way means the flag each host needs
// is written once, next to the reason that host needs it, instead of being copied
// into every tool that runs a command.
func (c ShellChoice) Args(script string) []string {
	return shellArgs(c, script)
}

// Usable reports whether this shell can be run at all.
//
// It is a question rather than an error so a caller can ask before it has a script to
// run, which is the point at which knowing there is no shell is still cheap.
func (c ShellChoice) Usable() bool { return c.Path != "" && c.Err == nil }

// ErrNoShell is a shell choice that was never filled in.
//
// It exists because a shell with no path and no reason of its own is the worst of
// both: a caller asking why gets nothing to say, and a caller passing the reason on
// to its own caller wraps a nil error and produces a message with a hole in it. So a
// choice that cannot be run always has something to explain itself with.
var ErrNoShell = errors.New("platform: no shell was chosen")

// Reason says why this shell cannot be run, and is never nil for a shell that cannot.
//
// The zero value of ShellChoice is a shell that cannot be run, so this is what makes
// the two agree. Everything that reports a missing shell should report it through
// here rather than through the field, so that there is no way to produce a complaint
// with nothing in it.
func (c ShellChoice) Reason() error {
	if c.Usable() {
		return nil
	}
	if c.Err != nil {
		return c.Err
	}
	if c.Path == "" {
		return ErrNoShell
	}
	return fmt.Errorf("%w: %s", ErrNoShell, c.Path)
}

// DefaultShell is the shell to use when the caller has not chosen one.
//
// The order is a preference order, and it is not arbitrary: the first choice is the
// one whose flags mean what a script written for a POSIX shell expects, and each
// fallback is a shell that exists on the host but is not what the script was written
// for. The last fallback exists so that the answer is never nothing, because a
// missing shell is a far worse failure than an unfamiliar one.
func DefaultShell() ShellChoice {
	return defaultShell()
}
