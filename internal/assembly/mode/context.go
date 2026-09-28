// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Tree limits keep the snapshot a fixed shape: a repository with ten thousand
// files must not push the conversation's standing context past what a model can
// hold, and a fixed bound keeps the bytes stable enough to be cached.
const (
	// SnapshotTreeDepth is how many directory levels the tree shows.
	SnapshotTreeDepth = 2
	// SnapshotEntriesPerDir is how many entries of one directory the tree lists.
	SnapshotEntriesPerDir = 24
	// SnapshotMaxEntries bounds the whole tree, so one wide repository cannot
	// dominate the prefix no matter how the depth is arranged.
	SnapshotMaxEntries = 120
)

// buildMarkers map the files whose presence says something about how the project
// is built and what it is written in. The detection is by name only, because the
// snapshot is standing context rather than an audit: naming the ecosystem is the
// part that changes how the model works, and reading every manifest to prove it
// would cost a walk of every directory for one adjective.
var buildMarkers = []struct {
	name  string
	label string
}{
	{"go.mod", "Go module"},
	{"go.work", "Go workspace"},
	{"package.json", "Node.js / npm"},
	{"deno.json", "Deno"},
	{"Cargo.toml", "Rust / Cargo"},
	{"pyproject.toml", "Python project"},
	{"requirements.txt", "Python (requirements)"},
	{"setup.py", "Python (setuptools)"},
	{"Gemfile", "Ruby / Bundler"},
	{"pom.xml", "Java / Maven"},
	{"build.gradle", "Java / Gradle"},
	{"CMakeLists.txt", "CMake"},
	{"Makefile", "Make"},
}

// skippedDirectories are the names a source tree snapshot never descends into.
// They hold dependency trees, build output and version control state: none of it
// is code the conversation edits, and listing any of it would drown the part of
// the tree a person means when they say "the project".
var skippedDirectories = map[string]struct{}{
	"node_modules":  {},
	"vendor":        {},
	"dist":          {},
	"build":         {},
	"out":           {},
	"target":        {},
	"coverage":      {},
	"__pycache__":   {},
	".venv":         {},
	"venv":          {},
	"bin":           {},
	"obj":           {},
	".idea":         {},
	".next":         {},
	".turbo":        {},
	".gradle":       {},
	".pytest_cache": {},
	".mypy_cache":   {},
	".ruff_cache":   {},
	"Pods":          {},
	"DerivedData":   {},
}

// Snapshot renders the standing context of a project: where it is, what it is
// built with, and the shape of its top directories.
//
// The result is generated once per conversation and then kept, so the bytes are
// the same on every turn: a snapshot that churned every turn would sit in front
// of the message history and break the prefix cache for the whole session.
// Sorting, bounded walking and name-only detection are all in service of that
// stability.
//
// A project that cannot be read yields an empty snapshot rather than an error:
// standing context that fails to arrive costs a little accuracy, while a
// conversation that fails to start costs the turn.
func Snapshot(root string) string {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("Project directory: ")
	builder.WriteString(filepath.Clean(root))
	// The host is part of the standing context because the tools answer to it:
	// a shell command, a build flag and a path separator are all questions the
	// operating system answers before the project does.
	builder.WriteString("\n\nHost system: ")
	builder.WriteString(systemDescription())
	if ecosystems := detectEcosystems(root); len(ecosystems) > 0 {
		builder.WriteString("\n\nLanguage and build system:\n")
		for _, label := range ecosystems {
			builder.WriteString("- ")
			builder.WriteString(label)
			builder.WriteString("\n")
		}
	}
	if tree := directoryTree(root); tree != "" {
		builder.WriteString("\nDirectory tree (top ")
		builder.WriteString(levelWord(SnapshotTreeDepth))
		builder.WriteString(" levels):\n")
		builder.WriteString(tree)
	}
	return builder.String()
}

// levelWord renders the depth of the tree for the header line.
func levelWord(levels int) string {
	if levels == 1 {
		return "one"
	}
	return strconv.Itoa(levels)
}

// detectEcosystems names the ecosystems whose marker files sit at the root.
func detectEcosystems(root string) []string {
	found := make([]string, 0, len(buildMarkers))
	seen := make(map[string]struct{}, len(buildMarkers))
	for _, marker := range buildMarkers {
		info, err := os.Stat(filepath.Join(root, marker.name))
		if err != nil || info.IsDir() {
			continue
		}
		if _, duplicate := seen[marker.label]; duplicate {
			continue
		}
		seen[marker.label] = struct{}{}
		found = append(found, marker.label)
	}
	sort.Strings(found)
	return found
}

// directoryTree lists the project's directories to a bounded depth, skipping
// everything that is not source the conversation would touch.
//
// The entries are directories only. Listing files as well would double the size
// of the snapshot for text the read and glob tools answer better, and the
// directory names are what orient the model in the tree.
func directoryTree(root string) string {
	entries := make([]string, 0, SnapshotMaxEntries)
	overflow := walkTree(root, "", SnapshotTreeDepth, &entries)
	if len(entries) == 0 {
		return ""
	}
	sort.Strings(entries)
	var builder strings.Builder
	for _, entry := range entries {
		builder.WriteString("- ")
		builder.WriteString(entry)
		builder.WriteString("/\n")
	}
	if overflow {
		builder.WriteString("- (more directories not shown)\n")
	}
	// Each entry ends with a newline, so the caller can append a blank line
	// without deciding where the tree's own boundary was.
	return builder.String()
}

// walkTree collects directory paths under root, relative and slash-separated so
// a snapshot reads the same on every platform. It reports whether entries were
// left out.
func walkTree(root, prefix string, depth int, entries *[]string) bool {
	if depth <= 0 {
		return false
	}
	directory := joinPath(root, prefix)
	items, err := os.ReadDir(directory)
	if err != nil {
		// A directory that cannot be read is a directory the tree does not show.
		// The snapshot is context, not an audit, so a permission error costs the
		// branch rather than the whole walk.
		return false
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		name := item.Name()
		if !item.IsDir() || isSkippedName(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	shown := names
	overflow := false
	if len(shown) > SnapshotEntriesPerDir {
		shown = shown[:SnapshotEntriesPerDir]
		overflow = true
	}
	for _, name := range shown {
		if len(*entries) >= SnapshotMaxEntries {
			return true
		}
		relative := name
		if prefix != "" {
			relative = prefix + "/" + name
		}
		*entries = append(*entries, relative)
		if walkTree(root, relative, depth-1, entries) {
			overflow = true
		}
	}
	return overflow
}

// isSkippedName reports whether a directory name is one the snapshot never
// descends into. Hidden entries are skipped by the same rule, because a hidden
// directory is tooling rather than source, and tooling is not what the tree is
// for.
func isSkippedName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	_, skipped := skippedDirectories[name]
	return skipped
}

// joinPath joins a root and a slash-separated prefix without filepath.Join,
// because the prefix is a display path with its separators already chosen.
func joinPath(root, prefix string) string {
	if prefix == "" {
		return root
	}
	return root + string(os.PathSeparator) + prefix
}
