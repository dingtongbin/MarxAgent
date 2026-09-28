// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package web

// listRoots is where a person starts browsing on a machine without drive letters.
//
// A POSIX filesystem has one top and it is written the same way everywhere, so the
// answer is one entry and it is the root itself. There is no second question to ask,
// which is why this file is eleven lines and the Windows one is not.
func listRoots() ([]DirEntry, error) {
	return []DirEntry{{Name: "/", Path: "/", Dir: true}}, nil
}

// looksLikeABareDrive is always false here.
//
// A machine without drive letters has no such shape: a single letter is an ordinary
// directory name, and one that exists is exactly what somebody asked for.
func looksLikeABareDrive(string) bool { return false }
