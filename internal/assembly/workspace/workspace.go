// SPDX-License-Identifier: Apache-2.0

// Package workspace holds the tabs this installation has open.
//
// It is here rather than in the interface because a tab tree that lives in one
// window's memory is gone the moment that window is reloaded, and reloading a page
// is something people do constantly. The tree is the server's, so it comes back.
//
// It is one tree rather than one per window, and the identifier a window might
// otherwise send is deliberately not accepted. A per-window tree needs something
// on the browser to tell one window from another, and the only durable thing a
// browser can hold for that is a value stored on the machine — which is a task's
// state living somewhere it does not belong, cleared by a browser tidy-up, and
// different on the next machine. The same window on the next machine, or the same
// machine after a cleared storage, would come back to a different set of tabs than
// the one that was left.
//
// What that costs is that two windows show the same tabs. It is the price of not
// keeping a copy of the work on the browser, and it is a price worth paying: a
// window showing what another window did is odd, and a window that has forgotten
// what it was doing is worse.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The kinds of tab. A tab is either a conversation or one of the places a person
// goes rather than returns to.
const (
	KindSession  = "session"
	KindNew      = "new"
	KindSettings = "settings"
	KindHelp     = "help"
	KindSkills   = "skills"
)

// Kinds is every kind a tab can be, and a tab of any other kind is refused rather
// than stored: an unknown kind would be one nothing knows how to render, and
// storing it would put a tab in the strip that cannot be shown.
var Kinds = []string{KindSession, KindNew, KindSettings, KindHelp, KindSkills}

// Limits. A tab strip with a thousand tabs in it is a window nobody can read, and
// a request that would make one is a request that is not a window.
const (
	// MaxTabs is how many tabs may be open.
	MaxTabs = 64
	// maxIDLength bounds a tab's identifier, which reaches a file name.
	maxIDLength = 128
)

// fileName is the one file this is kept in.
const fileName = "tabs.json"

// ErrInvalidWorkspace reports a tab tree that cannot be stored.
var ErrInvalidWorkspace = errors.New("workspace: invalid tab tree")

// Tab is one entry in the strip.
type Tab struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// Tree is the whole strip.
type Tree struct {
	// Tabs are in the order the strip shows them.
	Tabs []Tab `json:"tabs"`
	// Active is the tab showing. It is empty only when the strip is empty.
	Active string `json:"active,omitempty"`
	// UpdatedAt is when it was last written, so a window can tell a tree that is
	// older than what it has from one that is newer.
	UpdatedAt time.Time `json:"updated_at"`
}

// Store keeps the one tree.
type Store struct {
	mu   sync.RWMutex
	path string
	tree *Tree
}

// Open prepares the store under a directory, creating it if it is not there.
func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: a directory is required", ErrInvalidWorkspace)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create %s: %w", root, err)
	}
	return &Store{path: filepath.Join(root, fileName)}, nil
}

// Get returns the strip.
//
// A tree that has never been written gets the new-task tab already open, because
// a window with nothing at all in it has no way to get started and the front page
// of this program is a place to type.
func (s *Store) Get() (Tree, error) {
	s.mu.RLock()
	cached := s.tree
	s.mu.RUnlock()
	if cached != nil {
		return cloneTree(cached), nil
	}
	stored, err := readTree(s.path)
	if err != nil {
		return Tree{}, err
	}
	if stored == nil {
		fresh := Tree{Tabs: []Tab{{ID: KindNew, Kind: KindNew}}, Active: KindNew}
		s.remember(&fresh)
		return cloneTree(&fresh), nil
	}
	s.remember(stored)
	return cloneTree(stored), nil
}

// Put stores the strip.
func (s *Store) Put(tree Tree) (Tree, error) {
	stored, err := Validate(tree)
	if err != nil {
		return Tree{}, err
	}
	s.remember(stored)
	if err := writeTree(s.path, stored); err != nil {
		return Tree{}, err
	}
	return cloneTree(stored), nil
}

func (s *Store) remember(tree *Tree) {
	s.mu.Lock()
	s.tree = tree
	s.mu.Unlock()
}

// Validate refuses a tree that would put something in the strip nothing can render
// or file nothing can name.
func Validate(tree Tree) (*Tree, error) {
	cleaned := Tree{Active: strings.TrimSpace(tree.Active)}
	if len(tree.Tabs) == 0 {
		return nil, fmt.Errorf("%w: a window must have at least one tab", ErrInvalidWorkspace)
	}
	if len(tree.Tabs) > MaxTabs {
		return nil, fmt.Errorf("%w: %d tabs is more than the %d a window can show",
			ErrInvalidWorkspace, len(tree.Tabs), MaxTabs)
	}
	seen := make(map[string]struct{}, len(tree.Tabs))
	activeSeen := false
	for _, tab := range tree.Tabs {
		kind := strings.TrimSpace(tab.Kind)
		if !validKind(kind) {
			return nil, fmt.Errorf("%w: %q is not a kind of tab", ErrInvalidWorkspace, tab.Kind)
		}
		identifier := strings.TrimSpace(tab.ID)
		if identifier == "" {
			return nil, fmt.Errorf("%w: a tab has no identifier", ErrInvalidWorkspace)
		}
		if len(identifier) > maxIDLength {
			return nil, fmt.Errorf("%w: a tab identifier is longer than %d", ErrInvalidWorkspace, maxIDLength)
		}
		if kind == KindSession {
			// A conversation tab names a conversation, so the name has to be one a
			// directory could hold. Anything else would be a tab pointing at nothing,
			// and the window would show a conversation that never existed.
			if strings.ContainsAny(identifier, `/\:`) || identifier == "." || identifier == ".." {
				return nil, fmt.Errorf("%w: %q is not a conversation identifier", ErrInvalidWorkspace, identifier)
			}
		}
		key := kind + "\x00" + identifier
		if _, duplicate := seen[key]; duplicate {
			// The same tab twice is what a window gets from two clicks landing at
			// once, and a strip with the same tab twice cannot be told apart.
			continue
		}
		seen[key] = struct{}{}
		cleaned.Tabs = append(cleaned.Tabs, Tab{ID: identifier, Kind: kind})
		if cleaned.Active == identifier {
			activeSeen = true
		}
	}
	if !activeSeen {
		// A tree whose active tab is not in it is a window with nothing showing.
		// The first tab is the one to show rather than none, because a window with
		// no content is a window that looks broken.
		cleaned.Active = cleaned.Tabs[0].ID
	}
	cleaned.UpdatedAt = time.Now().UTC()
	return &cleaned, nil
}

func validKind(kind string) bool {
	for _, known := range Kinds {
		if known == kind {
			return true
		}
	}
	return false
}

func readTree(path string) (*Tree, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("workspace: read %s: %w", path, err)
	}
	var tree Tree
	if err := json.Unmarshal(encoded, &tree); err != nil {
		// A file that cannot be read is a lost tab strip, not a reason to refuse
		// every request for ever. It is renamed rather than deleted, because
		// whatever wrote it may still be worth looking at.
		broken := path + ".broken"
		if renameErr := os.Rename(path, broken); renameErr == nil {
			return nil, fmt.Errorf("workspace: %s could not be read and was kept as %s: %w",
				path, filepath.Base(broken), err)
		}
		return nil, fmt.Errorf("workspace: %s could not be read: %w", path, err)
	}
	validated, err := Validate(tree)
	if err != nil {
		return nil, fmt.Errorf("workspace: %s is not a tab tree this build can use: %w", path, err)
	}
	return validated, nil
}

func writeTree(path string, tree *Tree) error {
	encoded, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: encode the tab tree: %w", err)
	}
	// Written whole and atomically: a tab tree cut off half way through would be a
	// window that comes back with some of its tabs, which is worse than one that
	// comes back with none and can be rebuilt.
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("workspace: write %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("workspace: write %s: %w", path, err)
	}
	return nil
}

func cloneTree(tree *Tree) Tree {
	cloned := Tree{Active: tree.Active, UpdatedAt: tree.UpdatedAt}
	cloned.Tabs = append([]Tab(nil), tree.Tabs...)
	return cloned
}
