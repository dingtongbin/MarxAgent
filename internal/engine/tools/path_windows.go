// SPDX-License-Identifier: Apache-2.0
//go:build windows

package tools

import (
	"path/filepath"
	"strings"
)

func pathWithin(root, target string) bool {
	root = strings.ToLower(filepath.Clean(root))
	target = strings.ToLower(filepath.Clean(target))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
