// SPDX-License-Identifier: Apache-2.0
//go:build windows

package tools

import (
	"fmt"

	"github.com/dingtongbin/MarxAgent/internal/engine/platform"
)

// shellArgs runs a script with the shell this host prefers.
//
// Which shell that is, and why, is the platform package's business; all this decides
// is that a shell the caller named wins, and that a host with no shell at all is
// reported rather than run with an empty command.
func shellArgs(config ShellConfig, script string) ([]string, error) {
	choice := platform.DefaultShell()
	if config.Shell != "" {
		choice = platform.ShellChoice{Path: config.Shell, Interactive: true}
	}
	if !choice.Usable() {
		return nil, fmt.Errorf("tools: no shell to run the script with: %w", choice.Reason())
	}
	return choice.Args(script), nil
}
