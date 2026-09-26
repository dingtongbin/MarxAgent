// SPDX-License-Identifier: Apache-2.0
//go:build windows

package platform

import (
	"path/filepath"
	"strings"
)

// caselessNames is true here because the host itself does not distinguish case.
const caselessNames = true

// normalizeForCompare puts a path into the one form comparisons are made in, folding
// case because this host does not distinguish it.
func normalizeForCompare(path string) string {
	return strings.ToLower(filepath.Clean(path))
}
