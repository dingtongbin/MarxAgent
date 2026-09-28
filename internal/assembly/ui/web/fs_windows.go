// SPDX-License-Identifier: Apache-2.0

//go:build windows

package web

import "os"

// listRoots is where a person starts browsing on Windows, which is the drives that can
// be opened.
func listRoots() ([]DirEntry, error) {
	letters := volumeLetters()
	entries := make([]DirEntry, 0, len(letters))
	for _, letter := range letters {
		// The separator is part of the name, and the path is the name: a drive is the
		// whole of it, and joining a child onto it gives "C:\name" rather than the
		// relative path "C:name".
		// The colon is part of the path, and leaving it out is not a smaller name but a
		// different thing altogether: "C" is a relative path, resolved against this
		// process's working directory, and a directory that is not there is answered
		// with an empty listing rather than an error. The row would open onto nothing
		// and look like an empty drive.
		name := string(letter) + `:\`
		entries = append(entries, DirEntry{Name: name, Path: name, Dir: true})
	}
	return entries, nil
}

// volumeLetters is the drive letters that can be opened right now.
//
// It asks the system for each of the twenty-six rather than calling the one call that
// would answer in a single step, because that one is a syscall this project does not
// make: the project is built with cgo disabled so that one binary cross-compiles to
// every platform, and a Windows-only syscall reachable from pure Go is not something to
// reintroduce for a letter on a drive.
//
// The cost of asking one at a time is twenty-six cheap stat calls, once, when a dialog
// opens, and the benefit is that the answer is the filesystem's own rather than a
// table this file maintains.
//
// The limit, stated rather than hidden: a drive letter the system knows about but that
// has no readable root — an empty card reader, a network drive that is not connected —
// does not appear, because the only way to see it without the syscall is to open it. A
// person whose removable drive is missing from the list has been told the truth about
// what can be opened, which is the question the list answers.
func volumeLetters() []rune {
	letters := make([]rune, 0, 4)
	for letter := 'A'; letter <= 'Z'; letter++ {
		root := string(letter) + `:\`
		if _, err := os.Stat(root); err == nil {
			letters = append(letters, letter)
		}
	}
	return letters
}

// looksLikeABareDrive says whether a path is a drive letter with nothing after it.
//
// The shape matters because of what Windows does with it. "C:" and "C" are not the top
// of that drive and not a path to anything: they are relative paths, resolved against
// the working directory of whichever process is asking, so a request carrying one is
// answered with a listing of somewhere under the server rather than with the drive
// that was meant. And a directory that does not exist is answered with an empty listing
// rather than an error, so the wrong answer arrives looking like a true one.
func looksLikeABareDrive(path string) bool {
	if len(path) != 1 && len(path) != 2 {
		return false
	}
	letter := path[0]
	if letter < 'A' || (letter > 'Z' && letter < 'a') || letter > 'z' {
		return false
	}
	return len(path) == 1 || path[1] == ':'
}
