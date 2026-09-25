// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package tools

import (
	"fmt"
	"os"
)

func shellArgs(config ShellConfig, script string) ([]string, error) {
	shell := config.Shell
	if shell == "" {
		if _, err := os.Stat("/bin/bash"); err == nil {
			shell = "/bin/bash"
		} else {
			shell = os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/sh"
			}
		}
	}
	if shell == "" {
		return nil, fmt.Errorf("tools: shell executable is empty")
	}
	return []string{shell, "-lc", script}, nil
}
