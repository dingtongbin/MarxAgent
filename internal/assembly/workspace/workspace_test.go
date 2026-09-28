// SPDX-License-Identifier: Apache-2.0

package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAWindowThatHasNeverBeenHereGetsTheNewSessionTab(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tree, err := store.Get()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(tree.Tabs) != 1 || tree.Tabs[0].Kind != KindNew {
		t.Fatalf("tree = %#v, want the new-session tab and nothing else", tree.Tabs)
	}
	if tree.Active != KindNew {
		t.Fatalf("active = %q, want the one tab there is", tree.Active)
	}
}

func TestTheStripComesBackAfterAWindowIsReloaded(t *testing.T) {
	// The whole reason this exists: a window that is reloaded has no memory, and a
	// tab tree that only lived in that memory is gone.
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wanted := Tree{
		Tabs: []Tab{
			{ID: "20260101-120000-abcd", Kind: KindSession},
			{ID: KindSettings, Kind: KindSettings},
			{ID: KindNew, Kind: KindNew},
		},
		Active: "20260101-120000-abcd",
	}
	if _, err := store.Put(wanted); err != nil {
		t.Fatalf("put: %v", err)
	}

	// A second store over the same directory is what a restart of the program is.
	restarted, err := Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	back, err := restarted.Get()
	if err != nil {
		t.Fatalf("get after the restart: %v", err)
	}
	if len(back.Tabs) != 3 {
		t.Fatalf("tabs = %#v, want the three that were stored", back.Tabs)
	}
	if back.Tabs[0].ID != "20260101-120000-abcd" || back.Tabs[2].ID != KindNew {
		t.Fatalf("tabs came back in a different order: %#v", back.Tabs)
	}
	if back.Active != wanted.Active {
		t.Fatalf("active = %q, want %q", back.Active, wanted.Active)
	}
}

func TestTheStripDoesNotDependOnWhoIsLooking(t *testing.T) {
	// A tree kept per window needs a value stored on the browser to tell one window
	// from another, and a value on the browser is state that a browser tidy-up
	// takes away. The same window on another machine would then come back to
	// different tabs, which is the whole thing this avoids.
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.Put(Tree{
		Tabs:   []Tab{{ID: "a", Kind: KindSession}},
		Active: "a",
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Every read is of the same one tree, whoever asks.
	for range 3 {
		back, err := store.Get()
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(back.Tabs) != 1 || back.Tabs[0].ID != "a" {
			t.Fatalf("the strip changed between reads: %#v", back.Tabs)
		}
	}
}

func TestTheStripIsOneFileAndNothingInItIsNamedAfterAWindow(t *testing.T) {
	// The identifier a window might send is not accepted, so nothing on disk can be
	// named after a browser.
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.Put(Tree{
		Tabs:   []Tab{{ID: KindNew, Kind: KindNew}},
		Active: KindNew,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	entries, err := os.ReadDir(store.path[:len(store.path)-len(fileName)])
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != fileName {
		t.Fatalf("the directory holds %#v, want one file named %s", entries, fileName)
	}
}

func TestATreeThatCouldNotBeShownIsRefused(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cases := []struct {
		name string
		tree Tree
	}{
		{"no tabs", Tree{}},
		{"a kind nothing can render", Tree{Tabs: []Tab{{ID: "x", Kind: "wormhole"}}}},
		{"a tab with no name", Tree{Tabs: []Tab{{Kind: KindSession}}}},
		{
			"a conversation tab naming a path",
			Tree{Tabs: []Tab{{ID: "../../secrets", Kind: KindSession}}},
		},
		{
			"more tabs than a window can show",
			Tree{Tabs: make([]Tab, MaxTabs+1)},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := store.Put(testCase.tree); err == nil {
				t.Fatalf("%#v was stored", testCase.tree)
			}
		})
	}
}

func TestATreeWithNoActiveTabShowsItsFirstOne(t *testing.T) {
	// A window with nothing showing is a window that looks broken, so a tree that
	// does not say what is showing is corrected rather than stored as it is.
	stored, err := Validate(Tree{Tabs: []Tab{
		{ID: "a", Kind: KindSession},
		{ID: "b", Kind: KindSession},
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if stored.Active != "a" {
		t.Fatalf("active = %q, want the first tab", stored.Active)
	}
}

func TestAnActiveTabThatIsNotInTheTreeIsCorrected(t *testing.T) {
	stored, err := Validate(Tree{
		Tabs:   []Tab{{ID: "a", Kind: KindSession}},
		Active: "somewhere-else",
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if stored.Active != "a" {
		t.Fatalf("active = %q, want a tab that is actually there", stored.Active)
	}
}

func TestTheSameTabTwiceIsStoredOnce(t *testing.T) {
	// Two clicks landing at once is how a strip gets the same tab twice, and a
	// strip with the same tab twice cannot be told apart.
	stored, err := Validate(Tree{Tabs: []Tab{
		{ID: "a", Kind: KindSession},
		{ID: "a", Kind: KindSession},
	}})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(stored.Tabs) != 1 {
		t.Fatalf("tabs = %#v, want one", stored.Tabs)
	}
}

func TestWhatWasHandedOutIsACopyRatherThanTheOneKept(t *testing.T) {
	// A caller that could reach through what it was given could change what the
	// next read shows.
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tree, err := store.Put(Tree{
		Tabs:   []Tab{{ID: "a", Kind: KindSession}},
		Active: "a",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	tree.Tabs[0].ID = "tampered"
	tree.Active = "tampered"

	back, err := store.Get()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if back.Tabs[0].ID != "a" || back.Active != "a" {
		t.Fatalf("what was handed out reached what was kept: %#v", back)
	}
}

func TestAFileThatCannotBeReadIsKeptAsideRatherThanLeftToFailForEver(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, fileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := store.Get(); err == nil {
		t.Fatal("a tree that could not be read was reported as an empty one")
	}
	// It is kept under a name of its own, because whatever wrote it may still be
	// worth looking at.
	if _, err := os.Stat(filepath.Join(root, fileName+".broken")); err != nil {
		t.Fatalf("the unreadable file was not kept: %v", err)
	}
	// And the next read works, rather than failing for ever.
	fresh, err := Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	tree, err := fresh.Get()
	if err != nil {
		t.Fatalf("get after the file was put aside: %v", err)
	}
	if len(tree.Tabs) != 1 {
		t.Fatalf("tree = %#v, want the new-session tab", tree.Tabs)
	}
}

func TestThereIsOnlyOneFilePerInstallation(t *testing.T) {
	// Anything named after a window would be a second copy of the same work on a
	// place it does not belong.
	root := t.TempDir()
	first, err := Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	second, err := Open(filepath.Join(root, "somewhere-else"))
	if err != nil {
		t.Fatalf("open elsewhere: %v", err)
	}
	if first.path == second.path {
		t.Skip("the two stores share a directory, so this says nothing")
	}
	if _, err := first.Put(Tree{Tabs: []Tab{{ID: "a", Kind: KindSession}}, Active: "a"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A store elsewhere is a different installation, and knows nothing about this
	// one — which is the point: it is named by where the program keeps its data,
	// not by who is looking.
	other, err := second.Get()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if other.Tabs[0].ID == "a" {
		t.Fatalf("a store in another directory showed this one's tabs: %#v", other.Tabs)
	}
}
