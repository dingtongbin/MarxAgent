// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func resolveExecutable(command string) (string, error) {
	if command == "" {
		return "", fmt.Errorf("%w: executable is required", ErrInvalidPolicy)
	}
	resolved := command
	if !filepath.IsAbs(resolved) {
		var err error
		resolved, err = exec.LookPath(resolved)
		if err != nil {
			return "", fmt.Errorf("sandbox: resolve executable %q: %w", command, err)
		}
	}
	resolved, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("sandbox: resolve executable %q: %w", command, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("sandbox: executable %q is not a regular file", command)
	}
	return filepath.Clean(resolved), nil
}
