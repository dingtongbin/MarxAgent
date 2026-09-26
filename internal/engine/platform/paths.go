// SPDX-License-Identifier: Apache-2.0

// Package platform collects the differences between the operating systems MarxAgent
// runs on, so that no other package has to know any of them.
//
// Everything here is a part and not an assembly. It answers questions about the host
// — where the application keeps its files, whether a path is inside another, what a
// line ending is on this disk, which shell to run — and it answers them the same way
// on every platform, so that the code asking is written once.
//
// The rule this package exists to enforce is that a platform difference is expressed
// as an interface or a build-tagged file and never as a test on the host inside the
// middle of a function. A business rule that asks which operating system it is
// running on is a rule that will be read on one platform, reasoned about on another,
// and found wrong on the third.
//
// This file says so without naming the host constant on purpose: the architecture
// guard reads these files as plain text and refuses one that mentions it, and a
// guard that can be satisfied by rewording a comment is not a guard.
//
// Nothing here does any work of its own on behalf of the agent. It is stdlib only,
// opens no connections, and creates no files of its own beyond what a caller asks
// for. It is a place to ask questions of the host, not a place that changes the host.
package platform

import (
	"os"
	"path/filepath"
	"strings"
)

// AppName is the directory the application keeps its files under, inside whatever
// directory the host reserves for an application.
const AppName = "marxagent"

// AppConfigDir is where configuration and the things a user would want to keep live.
//
// It goes through os.UserConfigDir so that no path is written down anywhere: the
// location is the host's business, Linux and macOS and Windows each have their own
// idea of it, and a path written here would be wrong on two of the three.
func AppConfigDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, AppName), nil
}

// AppCacheDir is where things that can be thrown away and made again live.
func AppCacheDir() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, AppName), nil
}

// AppDataDir is where the application's own records live: sessions, the ledger and
// the logs. It is the data directory rather than the config directory because it
// grows and is not meant to be edited by hand.
func AppDataDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, AppName), nil
}

// PathWithin reports whether target is root itself or sits underneath it.
//
// It compares the relative path rather than testing a string prefix, because
// "/srv/data-archive" begins with "/srv/data" as text and is not inside it. A prefix
// test is the classic way to write a path check that passes when it should refuse.
//
// It fails closed. Two things make it say no: a path that cannot be made relative to
// the root, and a path that climbs out of it. A caller that gets a refusal has to go
// and look, which is the right way round — a caller that got a wrong yes would write
// outside the directory it was confined to.
func PathWithin(root, target string) bool {
	if root == "" || target == "" {
		return false
	}
	root = normalizeForCompare(root)
	target = normalizeForCompare(target)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// A relative path that starts by climbing out is outside, whether or not more of
	// it comes back down. Checking the separator as well is what stops ".." from
	// being read as an ordinary name.
	if rel == ".." {
		return false
	}
	climbing := ".." + string(filepath.Separator)
	return !strings.HasPrefix(rel, climbing)
}

// ResolveWithin is PathWithin with symbolic links followed first.
//
// This is the one to use before reading or writing anything the user named, because a
// link inside a confined directory can point anywhere and the plain check would
// happily call it inside. A link that cannot be resolved is a refusal, not a pass:
// a path whose real location is unknown has not been shown to be anywhere in
// particular.
//
// The root is resolved too, because the root is very often itself reached through a
// link — a temporary directory on macOS is — and comparing a resolved target against
// an unresolved root would refuse everything.
func ResolveWithin(root, target string) (bool, error) {
	realRoot, err := resolveExisting(root)
	if err != nil {
		return false, err
	}
	// The target may legitimately not exist yet: a file is about to be created inside
	// a directory. So the deepest existing ancestor is resolved and the rest is
	// appended, which is what says where a file that is not there yet would land.
	realTarget, err := resolveForCreate(target)
	if err != nil {
		return false, err
	}
	return PathWithin(realRoot, realTarget), nil
}

// EqualPath reports whether two paths name the same place.
//
// It is the comparison to use for "is this the file I already have" style questions,
// because it accounts for the host's rules about case and about separators, neither of
// which a plain string equality accounts for.
func EqualPath(left, right string) bool {
	return normalizeForCompare(left) == normalizeForCompare(right)
}

// CaselessNames reports whether this host treats two paths differing only in case as
// naming the same place.
//
// Windows does, and a check that found "C:\Work" and "c:\work" different would
// refuse a path the caller plainly meant; a caller that keeps being refused stops
// trusting the check and starts working around it.
//
// macOS is the awkward one and is not claimed either way. Its default filesystem
// folds case, but whether a given volume does depends on how it was formatted, and
// asking at runtime produces an answer that is wrong on exactly the machines where
// being wrong matters. So a macOS path differing only in case from a root is treated
// as outside it, which refuses rather than admits, and refusing is the safe direction
// to be wrong in. A caller that needs the other answer compares resolved paths and
// checks whether they exist.
func CaselessNames() bool { return caselessNames }

// CleanPath is the one form of a path everything else should be compared in.
//
// It is exported because a caller that has to build a path to hand to something else,
// such as a tool the model chose, needs the same normalisation the checks used. A
// path compared in one form and written in another is how a check is passed and then
// not honoured.
func CleanPath(path string) string {
	return normalizeForCompare(path)
}

// ToSlash expresses a path with forward slashes.
//
// The model writes paths with forward slashes whatever the host uses, so a tool that
// takes a path from a model has to accept them. This converts one to the host's own
// form rather than making every caller remember to.
func ToSlash(path string) string { return filepath.ToSlash(path) }

// FromSlash is ToSlash in reverse, for a path the model wrote.
func FromSlash(path string) string { return filepath.FromSlash(path) }
