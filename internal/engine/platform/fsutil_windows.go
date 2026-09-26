// SPDX-License-Identifier: Apache-2.0
//go:build windows

package platform

import (
	"fmt"
	"io/fs"
	"os"
)

// setFileMode applies the read-only attribute and ignores the rest of the mode.
//
// Windows has no permission bits in the sense Unix has. What it has is a read-only
// attribute, which is one bit out of everything a caller might pass. Honouring that
// one and ignoring the rest is the most that can honestly be done, and it is worth
// doing: a file marked read-only that a caller asked to be read-only is a real
// outcome, and quietly doing nothing would leave the caller believing otherwise.
//
// The rest is refused rather than ignored silently. A caller that asked for 0600 and
// got an error can decide what to do; a caller that asked for 0600 and got no error
// will believe the file is private on a host where it is not.
func setFileMode(path string, mode fs.FileMode) error {
	if mode&0o200 == 0 {
		// The owner write bit is what the read-only attribute corresponds to.
		if err := os.Chmod(path, 0o444); err != nil {
			return fmt.Errorf("platform: mark %s read only: %w", path, err)
		}
		return nil
	}
	if err := os.Chmod(path, 0o666); err != nil {
		return fmt.Errorf("platform: make %s writable: %w", path, err)
	}
	return nil
}
