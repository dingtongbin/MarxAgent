// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"runtime"
	"sync"
)

// The host description is asked for once per process and then kept. Asking
// the platform what it is costs a kernel call, and a kernel call in the
// middle of a conversation's turn is a cost nothing needs to pay twice:
// the machine does not change version while the tools run.
var (
	systemOnce sync.Once
	systemText string
)

// systemDescription is the one line of host description a coding snapshot
// carries: the operating system and its version, and the architecture the
// tools will run on.
//
// The spelling is decided here so a platform file cannot drift into a second
// shape, and the platform files are where the version actually comes from,
// because "which version of the machine this is" is a platform question all
// the way down.
func systemDescription() string {
	systemOnce.Do(func() {
		systemText = systemLabel() + " " + systemVersion() + ", " + runtime.GOARCH
	})
	return systemText
}

// PrimeSystem warms the host description at startup, so a server that has
// just started is holding its answer before the first conversation names a
// turn: the first snapshot built then reads a cache rather than making its
// own call.
func PrimeSystem() {
	systemDescription()
}
