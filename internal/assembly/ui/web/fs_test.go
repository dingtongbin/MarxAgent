// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ask browses one path through the handler itself, which is where the routing, the
// token and the query are all real, and returns the listing or the status.
//
// A handler called without a socket is a handler with no listener to leak and no port
// to wait for, and the answer is the same one a browser gets.
func ask(t *testing.T, path string) (DirListing, int) {
	t.Helper()
	server, _ := newTestServer(t, nil)
	target := browsePath + "?path=" + neturl.QueryEscape(path)
	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, authorised(
		httptest.NewRequest(http.MethodGet, target, nil), server.Token()))
	var listing DirListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
	return listing, recorder.Code
}

// The tree a person walks is made of folders, so the reply is too.
//
// This is the whole contract of the listing: a file in it is a row that cannot be
// chosen, and a person paging through a home directory full of them is reading a list
// of things this dialog has no answer for.
func TestAListingHoldsFoldersAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a-folder", "b-folder", "nested"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a-file.go", "b-file.txt", "no-extension"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	listing, code := ask(t, dir)
	if code != http.StatusOK {
		t.Fatalf("browsing a directory = %d, want 200", code)
	}
	var names, paths []string
	for _, entry := range listing.Entries {
		if !entry.Dir {
			t.Fatalf("the listing holds a file: %#v", entry)
		}
		names = append(names, entry.Name)
		paths = append(paths, entry.Path)
	}
	want := []string{"a-folder", "b-folder", "nested"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("listing = %v, want %v", names, want)
	}
	// The path is the folder's whole name, not its last part. A caller that is given
	// only the name has to join it onto a parent, and joining needs the separator of
	// the machine — which is how "C:\Users" and "30978" become "C:\Users30978".
	for index, path := range paths {
		if want := filepath.Join(dir, wantNames[index]); path != want {
			t.Fatalf("entry %d path = %q, want %q", index, path, want)
		}
	}
}

var wantNames = []string{"a-folder", "b-folder", "nested"}

// A folder with nothing in it is an answer, not a failure.
//
// The distinction matters because a person who has just made an empty folder to work
// in is looking at exactly this, and an error for it would be telling them the folder
// they can see is not a folder.
func TestAnEmptyFolderIsAnEmptyListing(t *testing.T) {
	listing, code := ask(t, t.TempDir())
	if code != http.StatusOK {
		t.Fatalf("browsing an empty folder = %d, want 200", code)
	}
	if len(listing.Entries) != 0 {
		t.Fatalf("an empty folder listed %d entries: %v", len(listing.Entries), listing.Entries)
	}
	if listing.Error != "" {
		t.Fatalf("an empty folder reported %q", listing.Error)
	}
}

// A link to a folder is a way of getting to a folder.
//
// Hiding it would make a project that keeps one look like a project that does not,
// and the person looking for their own work would conclude it is not there. The read
// is the one that can fail, so a link that dangles is left out rather than listed as
// something it is not.
func TestALinkToAFolderIsListedAsAFolder(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "the-real-one")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "a-link")); err != nil {
		t.Skipf("this machine does not allow making links: %v", err)
	}
	// A link to nothing. Listed as neither, because there is nothing to open.
	if err := os.Symlink(filepath.Join(dir, "not-there"), filepath.Join(dir, "a-dangling-link")); err != nil {
		t.Skipf("this machine does not allow making links: %v", err)
	}

	listing, code := ask(t, dir)
	if code != http.StatusOK {
		t.Fatalf("browsing a directory with links = %d, want 200", code)
	}
	seen := map[string]bool{}
	for _, entry := range listing.Entries {
		seen[entry.Name] = entry.Dir
	}
	if !seen["a-link"] {
		t.Fatalf("a link to a folder was not listed: %v", listing.Entries)
	}
	if seen["a-dangling-link"] {
		t.Fatalf("a link to nothing was listed as a folder: %v", listing.Entries)
	}
}

// The top of the machine is a list of folders, or the one root there is.
//
// Which of those two it is depends on the machine and not on this code, so what is
// checked is the shape both answers have to have: something to open, and nothing above
// it to go up to.
func TestTheTopOfTheMachineIsMadeOfPlacesToStart(t *testing.T) {
	listing, code := ask(t, "")
	if code != http.StatusOK {
		t.Fatalf("browsing the top = %d, want 200", code)
	}
	if !listing.Roots {
		t.Fatal("the empty path was not answered as the top of the machine")
	}
	if len(listing.Entries) == 0 {
		t.Fatal("the top of the machine listed nothing to open")
	}
	if listing.Parent != nil {
		t.Fatalf("the top of the machine has a parent, %q", *listing.Parent)
	}
	for _, entry := range listing.Entries {
		if !entry.Dir {
			t.Fatalf("the top of the machine listed something that is not a place: %#v", entry)
		}
	}
	// On Windows those are drives, and each has to carry its separator: joining a child
	// onto "C" gives a relative path that resolves against the working directory of
	// whatever process is reading it, which is the one answer nobody meant.
	if runtime.GOOS == "windows" {
		for _, entry := range listing.Entries {
			if !strings.HasSuffix(entry.Name, `\`) {
				t.Fatalf("the drive %q has no separator", entry.Name)
			}
		}
	}
}

// A path that is not a folder is refused, and the refusal says which of the two
// reasons it is.
//
// A drive letter without its separator is the case worth the words: it is a relative
// path, and resolving it would answer with the server's own working directory, which is
// a path the person never asked about.
func TestAPathThatIsNotAFolderIsRefusedWithItsReason(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	listing, code := ask(t, file)
	if code != http.StatusBadRequest {
		t.Fatalf("browsing a file = %d, want 400", code)
	}
	if !strings.Contains(listing.Error, ErrNotDirectory.Error()) {
		t.Fatalf("browsing a file said %q", listing.Error)
	}
}

// A path that is not there is an empty folder rather than an error.
//
// Somebody typing a path into a box is usually part way through typing it, and a red
// error for one character still being wrong is a red error for something that has not
// been said yet.
func TestAPathThatIsNotThereIsAnEmptyFolder(t *testing.T) {
	listing, code := ask(t, filepath.Join(t.TempDir(), "not-made-yet"))
	if code != http.StatusOK {
		t.Fatalf("browsing a path that is not there = %d, want 200", code)
	}
	if len(listing.Entries) != 0 {
		t.Fatalf("a path that is not there listed %d entries", len(listing.Entries))
	}
	if listing.Error != "" {
		t.Fatalf("a path that is not there reported %q", listing.Error)
	}
}

// The order is name order, and it is the order a person reads in.
//
// Sorting is here rather than left to the filesystem because a filesystem's order is
// its own business: the same folder listed twice on two machines, or twice on one
// machine across a rebuild, would otherwise come back in two different orders and a
// person would have to re-find the row they were on.
func TestTheOrderIsNameOrderWhateverTheFilesystemSays(t *testing.T) {
	dir := t.TempDir()
	// Written in an order that is neither alphabetical nor reverse, with the case
	// differing too: a case-sensitive sort would put every capital first and read as a
	// different list from the one a person is looking at.
	for _, name := range []string{"Zebra", "apple", "Mango", "banana", "apple-pie"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	listing, _ := ask(t, dir)
	var names []string
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
	}
	want := []string{"apple", "apple-pie", "banana", "Mango", "Zebra"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", names, want)
	}
}

// A drive letter on its own is refused, and refused with words.
//
// The alternative is the one this test exists to stop: "C" is a relative path on
// Windows, so resolving it answers with a folder under the server's working directory,
// and a folder that is not there is answered with an empty listing rather than an
// error. The result is a tree that opens onto a drive nobody asked about and shows
// nothing in it, which is indistinguishable from a drive that is empty.
func TestABareDriveLetterIsRefusedRatherThanResolved(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("a machine without drive letters has no such path")
	}
	for _, path := range []string{"C", "C:", "d", "Z:"} {
		listing, code := ask(t, path)
		if code != http.StatusBadRequest {
			t.Fatalf("browsing %q = %d, want 400: %v", path, code, listing)
		}
		if !strings.Contains(listing.Error, "separator") {
			t.Fatalf("browsing %q said %q, which does not say what is wrong", path, listing.Error)
		}
		// The answer must not be a listing of somewhere under the working directory.
		if len(listing.Entries) != 0 {
			t.Fatalf("browsing %q listed %v", path, listing.Entries)
		}
	}
	// And a drive with its separator is the top of that drive, not the shape refused
	// here, which is why the refusal names the missing part rather than the drive.
	listing, code := ask(t, "C:\\")
	if code != http.StatusOK {
		t.Fatalf("browsing C:\\ = %d, want 200: %v", code, listing)
	}
	if len(listing.Entries) == 0 {
		t.Skip("this machine's C: has nothing in it")
	}
}

// The round trip a click takes, asserted end to end.
//
// The listing at the top of the machine is where every path in the tree comes from, so
// a wrong one poisons everything below it: the row is drawn from this answer, the click
// sends it back, and the listing that comes back is of wherever the server resolved the
// broken path to — somewhere under its own working directory, answered with an empty
// listing rather than an error. The failure looks exactly like an empty drive.
//
// This is why the test walks the road the dialog walks and not a shorter one: take the
// path the roots hand out, send it back, and expect the drive that was named.
func TestThePathARootHandsOutIsThePathThatOpensIt(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("a machine without drive letters has no such root")
	}
	roots, code := ask(t, "")
	if code != http.StatusOK {
		t.Fatalf("listing the top of the machine = %d, want 200", code)
	}
	if len(roots.Entries) == 0 {
		t.Skip("this machine has no readable drives")
	}
	for _, root := range roots.Entries {
		// A drive's path is the drive: letter, colon, separator. Anything else is a
		// relative path, and a relative path is answered with somewhere under this
		// process, which is the answer that looks like an empty drive.
		if len(root.Path) < 3 || root.Path[1] != ':' || root.Path[2] != os.PathSeparator {
			t.Fatalf("a drive root's path is %q, which is not a drive: a click on it opens somewhere under the server", root.Path)
		}
		listing, code := ask(t, root.Path)
		if code != http.StatusOK {
			t.Fatalf("opening %q = %d, want 200: %v", root.Path, code, listing)
		}
		if len(listing.Entries) == 0 {
			t.Fatalf("opening %q listed nothing: the path came back as a relative one and was answered from the server's working directory", root.Path)
		}
		for _, entry := range listing.Entries {
			if !strings.HasPrefix(entry.Path, root.Path) {
				t.Fatalf("a child of %q is %q, which is not under it", root.Path, entry.Path)
			}
		}
	}
}

// The round trip a click takes, asserted the way the dialog makes it.
//
// The drive the roots hand out is the path the click sends back, so a drive whose path
// is a relative one is answered from the server's working directory: a row that says
// "C" opens onto nothing, and the failure looks like an empty drive rather than an
// error. This walks the road the dialog walks — list the top, take what a root hands
// out, send it back — and asserts the drive that opens is the one that was named.
func TestARootsPathOpensTheDriveItNames(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("a machine without drive letters has no such root")
	}
	roots, code := ask(t, "")
	if code != http.StatusOK {
		t.Fatalf("the top of the machine = %d, want 200", code)
	}
	if len(roots.Entries) == 0 {
		t.Skip("no readable drive on this machine")
	}
	for _, root := range roots.Entries {
		// A drive is letter, colon and separator. A path without the colon is a
		// relative path, and a relative path is answered from wherever the server was
		// started — somewhere under its own directory, answered with an empty listing.
		if len(root.Path) < 3 || root.Path[1] != ':' || root.Path[2] != os.PathSeparator {
			t.Fatalf("a drive root's path is %q, which is not a drive: clicking it opens somewhere under the server", root.Path)
		}
		listing, code := ask(t, root.Path)
		if code != http.StatusOK {
			t.Fatalf("opening %q = %d, want 200: %v", root.Path, code, listing)
		}
		if len(listing.Entries) == 0 {
			t.Fatalf("opening %q listed nothing: the path is a relative one and was answered from the server's working directory", root.Path)
		}
		for _, entry := range listing.Entries {
			// A child of the drive is on the drive. Anything else came from somewhere
			// else, and the listing lied about where it looked.
			if !strings.HasPrefix(entry.Path, root.Path) {
				t.Fatalf("a child of %q is %q, which is not on that drive", root.Path, entry.Path)
			}
		}
	}
}
