// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package subagent

import (
	"path/filepath"
	"strings"
)

// pathWithin reports whether target is root or sits under it.
//
// The comparison is on the relative path rather than on a string prefix, because
// "/srv/data-archive" starts with "/srv/data" as a string and is not inside it.
func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
