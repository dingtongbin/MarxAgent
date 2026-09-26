// SPDX-License-Identifier: Apache-2.0
//go:build !windows

package platform

import (
	"fmt"
	"io/fs"
	"os"
)

// setFileMode applies a mode, which on this host is a real thing.
//
// The mode is passed to chmod rather than to the open call, because the open call
// only applies a mode to a file it is about to create and quietly leaves an existing
// one's alone. A caller that has decided what permissions a file should have means it
// for that file, new or not.
func setFileMode(path string, mode fs.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("platform: set the mode on %s: %w", path, err)
	}
	return nil
}
