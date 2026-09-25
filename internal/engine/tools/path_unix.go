// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package tools

import (
	"path/filepath"
	"strings"
)

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
