// SPDX-License-Identifier: Apache-2.0
//go:build !linux && !darwin && !windows

package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

func platformName() string {
	return "unsupported"
}

func platformAvailable() bool {
	return false
}

func platformCapabilities() Capabilities {
	return Capabilities{Backend: platformName()}
}

func platformValidatePolicy(normalizedPolicy) error {
	return nil
}

func defaultPath() string {
	return "/usr/bin:/bin"
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func startPlatform(context.Context, *Engine, []string, []string, string) (Process, error) {
	return nil, fmt.Errorf("%w: %s", ErrUnsupported, platformName())
}
