// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LineEnding is the pair of bytes a text file uses to end a line.
type LineEnding struct {
	// Name is what to call it: "lf" or "crlf".
	Name string
	// Text is the actual ending, without the newline a writer may add after it.
	Text string
}

var (
	// LF is the ending almost everything uses.
	LF = LineEnding{Name: "lf", Text: "\n"}
	// CRLF is what a file written on Windows carries.
	CRLF = LineEnding{Name: "crlf", Text: "\r\n"}
)

// DetectLineEnding reports which ending a file already uses.
//
// It is here because a file rewritten with the other ending is a change nobody asked
// for. A repository that ends its lines one way and has a few files the other way is
// telling you something about those files, and an editor that normalised them would
// bury that in a diff nobody reads.
//
// A file with no line ending at all is reported as LF, because that is what the next
// line written into it will be, and a single line file has nothing to preserve.
func DetectLineEnding(path string) (LineEnding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LF, fmt.Errorf("platform: read %s to find its line endings: %w", path, err)
	}
	return DetectLineEndingIn(data), nil
}

// DetectLineEndingIn reports which ending some bytes already use.
//
// It is separate from the file version because the same question is asked of text
// that is being written as well as of text on disk, and answering it from the bytes
// means the answer is the same either way.
func DetectLineEndingIn(data []byte) LineEnding {
	// Only the first line ending decides, because a file with mixed endings has been
	// produced by something that edited part of it, and its overall habit is better
	// described by where it starts than by a count over the whole file.
	if index := strings.IndexByte(string(data), '\n'); index > 0 && data[index-1] == '\r' {
		return CRLF
	}
	return LF
}

// ApplyLineEnding rewrites text to end its lines the given way.
//
// A caller editing a file is expected to read the ending, hand the whole file to
// whatever is editing it, and put the ending back. Doing it this way means the edit
// itself never has to know, so there is one place that knows and not one per tool.
func ApplyLineEnding(text string, ending LineEnding) string {
	if ending.Name == CRLF.Name {
		// Normalise first, so text that already has a mix comes out uniform rather
		// than gaining a second ending in front of the ones it had.
		text = strings.ReplaceAll(text, "\r\n", "\n")
		return strings.ReplaceAll(text, "\n", ending.Text)
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return text
}

// resolveExisting follows a path all the way to what it really names.
func resolveExisting(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("platform: resolve %s: %w", path, err)
	}
	return resolved, nil
}

// resolveForCreate follows a path that need not exist yet.
//
// A file about to be written is not there, so there is nothing to resolve at the
// bottom. The deepest part that does exist is resolved instead and the rest is
// appended, which is what answers the question actually being asked: if this path
// were created, where would it land.
func resolveForCreate(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	parent := filepath.Dir(path)
	if parent == path {
		// The path is already its own parent, which means it is a root and there is
		// nothing above it to resolve.
		return path, nil
	}
	resolvedParent, err := resolveForCreate(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}

// IsLink reports whether a path is a symbolic link rather than the thing it names.
func IsLink(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("platform: inspect %s: %w", path, err)
	}
	return info.Mode()&fs.ModeSymlink != 0, nil
}

// SetFileMode applies a file mode, and on a host that has no modes it does nothing.
//
// The two hosts disagree about whether a file's permissions are a real thing. Unix
// has them and they matter; Windows has an attribute that behaves like one for a
// handful of flags and ignores the rest. Writing a caller that had to know that would
// put a host test in every file-writing path, so the difference is settled here and
// the caller sets a mode and lets the host decide what that means.
func SetFileMode(path string, mode fs.FileMode) error {
	return setFileMode(path, mode)
}

// DefaultFileMode is the mode a file the application creates is given.
//
// It is owner-only, because a session transcript and an audit ledger are the user's
// and not every account on a shared machine should read them.
const DefaultFileMode fs.FileMode = 0o600

// DefaultDirMode is the mode a directory the application creates is given.
const DefaultDirMode fs.FileMode = 0o700

// EnsureDir creates a directory and everything above it, and says so is fine if it
// is already there.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, DefaultDirMode); err != nil {
		return fmt.Errorf("platform: create the directory %s: %w", path, err)
	}
	// MkdirAll leaves an existing directory's mode alone, which is right: the caller
	// did not ask to change a directory somebody else made.
	return nil
}

// CreateDirs creates a list of directories, and reports the first that would not.
func CreateDirs(paths ...string) error {
	for _, path := range paths {
		if err := EnsureDir(path); err != nil {
			return err
		}
	}
	return nil
}

// ErrSymlinkForbidden is an attempt to create a symbolic link where the design does
// not allow one.
var ErrSymlinkForbidden = errors.New("platform: creating a symbolic link is not allowed here")

// WriteFile refuses to write through a link.
//
// Reading through a link is fine and is what links are for. Writing through one
// means the bytes land somewhere other than where the caller was told, and where that
// is depends on a file the caller did not choose and cannot see. So a link is read
// through and never written through, on every host, because the two are not the same
// risk and only one of them is avoidable.
func WriteFile(path string, data []byte, mode fs.FileMode) error {
	if link, err := IsLink(path); err == nil && link {
		return fmt.Errorf("%w: %s", ErrSymlinkForbidden, path)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("platform: write %s: %w", path, err)
	}
	return nil
}

// ReadLinkTarget reports what a symbolic link points at, without following it further.
//
// It exists so a caller that has just refused to write through a link can say where
// the link was aiming, which is the difference between a refusal somebody can
// understand and one they can only guess at.
func ReadLinkTarget(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("platform: read the link %s: %w", path, err)
	}
	return target, nil
}
