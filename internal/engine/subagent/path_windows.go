// SPDX-License-Identifier: Apache-2.0

//go:build windows

package subagent

import (
	"path/filepath"
	"strings"
)

// pathWithin reports whether target is root or sits under it.
//
// Both sides are folded to lower case first, because Windows and macOS treat paths
// case insensitively while Linux does not. A containment check that is exact on one
// platform and approximate on another is a check that passes on the platform where
// it is exact, which is how a template ends up read from outside its own directory
// on exactly the machines nobody tests on.
func pathWithin(root, target string) bool {
	root = strings.ToLower(filepath.Clean(root))
	target = strings.ToLower(filepath.Clean(target))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
