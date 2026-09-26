// SPDX-License-Identifier: Apache-2.0

package tools

import "github.com/dingtongbin/MarxAgent/internal/engine/platform"

// pathWithin reports whether target is root or sits under it.
//
// It is a call rather than a copy because this is a check that stands between a
// confined tool and the rest of the machine. Three copies of it were three chances to
// fix it in two of them, and the one left behind would have gone on deciding the same
// question a different way. The host's own rules about case and separators live in
// the platform package, which is where they are written down once.
func pathWithin(root, target string) bool {
	return platform.PathWithin(root, target)
}
