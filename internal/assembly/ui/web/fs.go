// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// The path the interface asks about what is on disk.
//
// A browser is not entitled to know where it is on the machine, so this is the only way
// a path reaches it. It is a GET with no side effect, which is what makes it safe to
// leave behind the same token as everything else rather than behind a second check:
// asking what is in a directory changes nothing, and the answer is the caller's own
// filesystem that they could have read themselves.
const browsePath = "/v1/fs/browse"

// gitPath is where the interface asks what git sees in a project.
//
// It is a read like the directory listing beside it: two read-only commands run
// inside the project — the status and the recent commits — and nothing else. The
// caller's own machine, behind the same token; the answer is what a person in
// that directory could have run themselves.
const gitPath = "/v1/fs/git"

// ErrNotDirectory is what a path that is not a directory answers with.
//
// It is its own error rather than a formatted string because the interface has to tell
// this apart from "there is nothing there" and from "you may not look": the first is a
// thing to fix, the second is an empty list, and the third is not the caller's to fix.
var ErrNotDirectory = errors.New("not a directory")

// DirEntry is one folder in a directory listing.
//
// There is no size, because a folder does not have one that means anything, and no
// `dir` flag in the payload's contract either — see readDir for why only folders are
// ever sent. Dir is kept because the interface reads it rather than assuming, and a
// payload that says what it is does not depend on the reader having read this file.
type DirEntry struct {
	Name string `json:"name"`
	// Path is where this folder is, whole.
	//
	// It is sent rather than assembled by the caller because a path is the server's to
	// know: joining a child onto its parent needs the separator of the machine, and a
	// caller that guesses it produces `C:\Users30978` — a path that does not exist,
	// made out of two halves that were both right.
	Path     string `json:"path"`
	Dir      bool   `json:"dir"`
	Size     int64  `json:"size,omitempty"`
	Modified string `json:"modified,omitempty"`
}

// DirListing is what a directory answers with.
type DirListing struct {
	// Roots is true when the request asked for the top of the machine rather than for a
	// directory, and the entries are the places one starts from. It is a separate field
	// rather than an empty path because an empty path and the root are the same string
	// on a POSIX machine and the interface has to tell them apart to know whether it is
	// looking at drive letters or at a folder.
	Roots bool `json:"roots"`
	// Path is where this listing is. For a root listing it is empty.
	Path string `json:"path"`
	// Parent is the directory above, or nil at the top. The interface uses it to decide
	// whether "up" does anything, which it must not do at the top.
	Parent  *string    `json:"parent"`
	Entries []DirEntry `json:"entries"`
	Error   string     `json:"error,omitempty"`
}

// handleBrowse lists the folders in one directory.
//
// The listing is by name, case-insensitively: name order because a list that is in
// whatever order the filesystem happened to return is a list a person has to read
// rather than scan.
func (s *Server) handleBrowse(c *gin.Context) {
	requested := c.Query("path")

	// Every request is written down, because the alternative is a tree in a window that
	// cannot explain itself: a row that opens and never fills in looks identical whether
	// the request never left, the answer was lost, or the answer was refused.
	//
	// Through slog, like everything else in the project: the fields are frozen, and a
	// second format would make the set worthless.
	slog.Info("browse", slog.String("component", "fs"), slog.String("path", requested))

	if isRootRequest(requested) {
		roots, err := listRoots()
		if err != nil {
			c.JSON(http.StatusInternalServerError, DirListing{Error: err.Error()})
			return
		}
		c.JSON(http.StatusOK, DirListing{Roots: true, Path: "", Parent: nil, Entries: roots})
		return
	}

	// Refused before it is resolved, because resolving it is what turns a drive letter
	// into a directory under this process's working directory — and an answer about
	// somewhere nobody asked about is worse than a refusal, especially when a
	// directory that is not there is answered with an empty listing rather than an
	// error, which makes the wrong answer look like the right one.
	if looksLikeABareDrive(strings.TrimSpace(requested)) {
		c.JSON(http.StatusBadRequest, DirListing{
			Path:  requested,
			Error: "a drive letter needs a separator and a name, for example D:\\code",
		})
		return
	}

	absolute, err := filepath.Abs(requested)
	if err != nil {
		c.JSON(http.StatusBadRequest, DirListing{Error: err.Error()})
		return
	}
	absolute = filepath.Clean(absolute)

	info, err := os.Stat(absolute)
	if err != nil {
		// A path that is not there is an empty listing rather than an error. A person
		// typing a path into a box and one character of it being wrong should see an
		// empty folder, not a red error for something they have not finished typing.
		if os.IsNotExist(err) {
			c.JSON(http.StatusOK, DirListing{Path: absolute, Parent: parentOf(absolute), Entries: []DirEntry{}})
			return
		}
		c.JSON(http.StatusForbidden, DirListing{Path: absolute, Error: err.Error()})
		return
	}
	if !info.IsDir() {
		// A bare drive letter arrives here rather than being treated as the top of that
		// drive, and the message says which of the two things it is so that a person who
		// typed it knows to add a separator rather than wondering why the path did not
		// go where it pointed.
		c.JSON(http.StatusBadRequest, DirListing{
			Path:  absolute,
			Error: ErrNotDirectory.Error() + " (a drive letter needs a separator, for example D:\\)",
		})
		return
	}

	entries, err := readDir(absolute)
	if err != nil {
		c.JSON(http.StatusForbidden, DirListing{Path: absolute, Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, DirListing{
		Path:    absolute,
		Parent:  parentOf(absolute),
		Entries: entries,
	})
}

// GitView is what a project answers the git question with.
type GitView struct {
	Root string `json:"root"`
	// Branch is the line the status answers with first: where this project is
	// and, when there is one, what it is ahead of.
	Branch string `json:"branch,omitempty"`
	// Status is the working tree's own list, one change per line, as git
	// writes it. Empty means the tree is clean.
	Status string `json:"status,omitempty"`
	// Log is the recent history, newest first, as git writes it.
	Log   string `json:"log,omitempty"`
	Error string `json:"error,omitempty"`
}

// handleGit reads what git sees in one project.
//
// The budget bounds the reads rather than trusting them: a git in a networked
// project can pause on a refresh, and a panel that waits on git is a panel
// that holds a request open for as long as the wait takes. Read-only commands
// only, inside the directory the caller named.
func (s *Server) handleGit(c *gin.Context) {
	requested := c.Query("path")

	slog.Info("git status", slog.String("component", "fs"), slog.String("path", requested))

	absolute, err := filepath.Abs(strings.TrimSpace(requested))
	if err != nil {
		c.JSON(http.StatusBadRequest, GitView{Error: err.Error()})
		return
	}
	absolute = filepath.Clean(absolute)
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		c.JSON(http.StatusBadRequest, GitView{Root: absolute, Error: "not a directory"})
		return
	}

	view := GitView{Root: absolute}
	ctx, cancel := context.WithTimeout(c.Request.Context(), gitBudget)
	defer cancel()

	if out, err := runGit(ctx, absolute, "status", "--porcelain=v1", "-b"); err != nil {
		view.Error = err.Error()
		c.JSON(http.StatusOK, view)
		return
	} else {
		view.Branch, view.Status = porcelainHead(out), porcelainRest(out)
	}
	if out, err := runGit(ctx, absolute, "log", "--oneline", "-20"); err == nil {
		view.Log = out
	}
	c.JSON(http.StatusOK, view)
}

// porcelainHead lifts the branch line out of the status output, which git puts first.
func porcelainHead(out string) string {
	if line, _, found := strings.Cut(out, "\n"); found && strings.HasPrefix(line, "## ") {
		return strings.TrimSpace(strings.TrimPrefix(line, "## "))
	}
	return ""
}

// porcelainRest is the status without the branch line, trimmed of the blank the
// cut leaves behind.
func porcelainRest(out string) string {
	_, rest, found := strings.Cut(out, "\n")
	if !found {
		return ""
	}
	return strings.TrimSpace(rest)
}

// gitBudget bounds the two reads this endpoint makes.
const gitBudget = 5 * time.Second

// runGit runs one read-only git command inside a directory and returns its output.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	command, cancel := context.WithTimeout(ctx, gitBudget)
	defer cancel()
	executed := exec.CommandContext(command, "git", args...)
	executed.Dir = dir
	out, err := executed.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// isRootRequest says whether this is a request for the top of the machine.
//
// Only the empty path, and the POSIX root written out. A drive letter with a separator
// is deliberately not one of them: "D:\" is a directory that happens to be a whole drive,
// and answering it with the list of drives means a person who clicked D can never get
// into D. The empty path is what the interface sends to ask "where do I start", and the
// two answers to that question are handled in listRoots rather than here.
//
// A bare "D:" with no separator is not a root either, for the other reason: it is a
// relative path meaning the current directory on that drive, and resolving it would
// answer with the server's own working directory. It is rejected as not-a-directory
// rather than guessed at.
func isRootRequest(path string) bool {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return true
	}
	// The POSIX root, which somebody with a path bar open will type. On Windows the
	// separator is a backslash and this never matches, which is right: "\" is a
	// directory on the current drive, not the top of anything.
	return trimmed == "/" && filepath.Separator == '/'
}

// parentOf is the directory above, or nil when there is not one.
func parentOf(path string) *string {
	parent := filepath.Dir(path)
	if parent == path {
		return nil
	}
	return &parent
}

// readDir lists the directories in one directory, by name.
//
// Only directories. The question this answers is "which folder", and a file is not a
// candidate for an answer: choosing one would be a path with no working directory in
// it. Sending them anyway would mean the interface has to know which of the two kinds
// it was sent in order to hide the half it does not want, and that knowledge is a
// thing that can be wrong — a row that is neither a folder nor nothing is a folder
// somebody cannot open. So the half that is not wanted is never sent, and the cost is
// a few bytes in a reply nobody has to filter.
//
// A symlink is listed as whatever it points at, because a link to a folder is a way of
// getting to a folder and hiding it would make a project that uses one look like a
// project that does not. The read is the one that can fail, and a link that dangles
// is left out rather than listed as something it is not.
func readDir(absolute string) ([]DirEntry, error) {
	handle, err := os.ReadDir(absolute)
	if err != nil {
		return nil, err
	}
	entries := make([]DirEntry, 0, len(handle))
	for _, item := range handle {
		isDir := item.IsDir()
		if !isDir && item.Type()&os.ModeSymlink != 0 {
			// os.ReadDir reports what the directory entry says, and a link says it is a
			// link. What it points at has to be asked, and this is the only place in the
			// project that follows one: a link that loops is not a problem here, because
			// a directory is only read when a person opens it and no listing walks
			// downwards on its own.
			info, err := os.Stat(filepath.Join(absolute, item.Name()))
			isDir = err == nil && info.IsDir()
		}
		if !isDir {
			continue
		}
		entry := DirEntry{Name: item.Name(), Path: filepath.Join(absolute, item.Name()), Dir: true}
		if info, err := item.Info(); err == nil {
			entry.Modified = info.ModTime().UTC().Format(time.RFC3339)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(a, b int) bool {
		return strings.ToLower(entries[a].Name) < strings.ToLower(entries[b].Name)
	})
	return entries, nil
}
