// SPDX-License-Identifier: Apache-2.0

//go:build windows

package mode

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// systemLabel is the family name a person calls this machine by.
func systemLabel() string {
	return "Windows"
}

// systemVersion asks the kernel for the version rather than the registry,
// because the registry values lie about the product they serve and the kernel
// call answers what the tools will actually run on. Windows 11 is Windows 10
// build 22000 and later; everything below is named for its major release.
func systemVersion() string {
	version := windows.RtlGetVersion()
	if version == nil {
		return ""
	}
	major, minor, build := version.MajorVersion, version.MinorVersion, version.BuildNumber
	switch {
	case major >= 10 && build >= 22000:
		return "11 (build " + uint32Text(build) + ")"
	case major >= 10:
		return "10 (build " + uint32Text(build) + ")"
	case major == 6 && minor == 3:
		return "8.1"
	case major == 6 && minor == 2:
		return "8"
	case major == 6 && minor == 1:
		return "7"
	default:
		return uint32Text(major) + "." + uint32Text(minor)
	}
}

// uint32Text is the plain decimal spelling of a version number.
func uint32Text(value uint32) string {
	return fmt.Sprintf("%d", value)
}
