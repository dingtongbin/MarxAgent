// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package platform

import "path/filepath"

// caselessNames is false here because a Unix filesystem really does distinguish
// /Root from /root, and folding them would be a lie a confinement check would then
// rely on.
const caselessNames = false

// normalizeForCompare puts a path into the one form comparisons are made in.
//
// On this host that is cleaning alone. Separators are already the host's own, because
// filepath.Clean rewrites them, and case is left exactly as written.
func normalizeForCompare(path string) string {
	return filepath.Clean(path)
}
