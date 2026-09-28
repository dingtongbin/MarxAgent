// SPDX-License-Identifier: Apache-2.0

package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// staticAssets is the built interface, compiled into the binary.
//
// Embedding rather than serving from a directory is what lets the whole thing be one
// file with nothing beside it. It is also what makes the offline promise real: there
// is nothing to fetch from a CDN, because there is no address to fetch from. A page
// that asked a remote host for a font or a script would work on a machine with a
// network and fail on one without, which is the opposite of what this is for.
//
//go:embed all:static
var staticAssets embed.FS

// StaticHandler serves the interface from the copy inside the binary.
//
// This is the shipping path and the default. The files are in the binary, they are
// the same on every machine, and nothing needs to be beside the program.
func StaticHandler() (http.Handler, error) {
	assets, err := fs.Sub(staticAssets, "static")
	if err != nil {
		return nil, fmt.Errorf("web: the interface is not in the binary: %w", err)
	}
	return fileHandler(assets, false), nil
}

// StaticHandlerFromDir serves the interface from a directory on disk.
//
// This is the development path. Embedding means a change to the interface needs the
// program rebuilt before anybody can see it, which turns a two second edit into a
// build and a restart, and the person doing the work waits on the toolchain instead
// of on their own change. Reading from disk removes that wait: the program keeps
// running and the browser reloads.
//
// The difference in caching is the other half of it. Built files are fingerprinted,
// so a browser may keep one for as long as it exists; files being edited are not, so
// a browser that kept them would show yesterday's interface while the file on disk
// said something else. Everything served from disk is therefore revalidated.
func StaticHandlerFromDir(dir string) (http.Handler, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("web: the interface directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("web: %s is not a directory", dir)
	}
	return fileHandler(os.DirFS(dir), true), nil
}

// fileHandler serves a set of files, whether they came from the binary or from disk.
func fileHandler(assets fs.FS, revalidate bool) http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A built interface is fingerprinted by Vite, so an asset under /assets can be
		// cached for as long as it exists: a change to the source produces a new file
		// name, so a cached one can never be stale. The entry document is the opposite
		// case, because its name never changes and it is what points at the new ones.
		switch {
		case revalidate:
			// Being edited, so a browser must ask. A stale interface while the file on
			// disk says something else is the single most confusing thing a developer
			// can be shown.
			w.Header().Set("Cache-Control", "no-store")
		case isFingerprinted(r.URL.Path):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// isFingerprinted reports whether a path is one of the build's hashed files.
//
// The test is the directory rather than the file name, because that is the part that
// is true of every hashed asset and of nothing else. Guessing at a name pattern would
// one day cache an unhashed file for a year.
func isFingerprinted(urlPath string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	return strings.HasPrefix(clean, "/assets/")
}

// assets is the handler for the interface, from the binary or from disk depending on
// how the server was told to serve it.
func (s *Server) assets() (http.Handler, error) {
	if s.config.WebDir != "" {
		return StaticHandlerFromDir(s.config.WebDir)
	}
	return StaticHandler()
}
