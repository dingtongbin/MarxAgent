// SPDX-License-Identifier: Apache-2.0
//go:build windows

package tools

import (
	"fmt"
	"os/exec"
)

func shellArgs(config ShellConfig, script string) ([]string, error) {
	shell := config.Shell
	if shell == "" {
		var err error
		shell, err = exec.LookPath("pwsh")
		if err != nil {
			shell, err = exec.LookPath("powershell")
			if err != nil {
				return nil, fmt.Errorf("tools: no PowerShell executable found: %w", err)
			}
		}
	}
	return []string{shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script}, nil
}
