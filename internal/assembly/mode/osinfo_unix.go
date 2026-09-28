// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package mode

import (
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// systemLabel is the family name a person calls this machine by. Linux and
// the BSDs are called by their own names; macOS is what a person calls
// darwin, so the kernel family name is carried in the version instead.
func systemLabel() string {
	if runtime.GOOS == "darwin" {
		return "macOS"
	}
	return runtime.GOOS
}

// systemVersion reads the kernel release from uname, which is the version the
// shell and the toolchain answer to. The release is shown as the kernel spells
// it, because paraphrasing a distro's release string is a chance to be wrong
// about the exact thing this line exists to say.
func systemVersion() string {
	var name unix.Utsname
	if err := unix.Uname(&name); err != nil {
		return runtime.GOOS
	}
	release := strings.TrimRight(string(name.Release[:]), "\x00")
	release = strings.TrimSpace(release)
	if release == "" {
		return runtime.GOOS
	}
	if runtime.GOOS == "darwin" {
		return "darwin " + release
	}
	return release
}
