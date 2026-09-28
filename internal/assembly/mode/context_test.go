// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSnapshotDetectsEcosystemsAndTrees(t *testing.T) {
	project := newTestProject(t)
	writeProjectFile(t, project, "go.mod", "module example.com/project\n")
	writeProjectFile(t, project, "package.json", `{"name":"project"}`)
	if err := os.MkdirAll(filepath.Join(project, "internal", "engine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "node_modules", "left-pad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "vendor", "example"), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot(project)
	if !strings.Contains(snapshot, "Project directory: ") {
		t.Fatalf("snapshot has no project line: %q", snapshot)
	}
	if !strings.Contains(snapshot, "Go module") || !strings.Contains(snapshot, "Node.js / npm") {
		t.Fatalf("the ecosystems were not detected: %q", snapshot)
	}
	// The tree is directories only, and the parts of the tree that are not
	// source are not in it: a dependency folder in the standing context is a
	// folder the model will read as if it were project code.
	if !strings.Contains(snapshot, "- internal/") || !strings.Contains(snapshot, "- internal/engine/") {
		t.Fatalf("the tree is missing its directories: %q", snapshot)
	}
	if strings.Contains(snapshot, "node_modules") || strings.Contains(snapshot, "vendor") {
		t.Fatalf("the tree lists directories it must skip: %q", snapshot)
	}
}

// A snapshot without markers and without directories is still a snapshot of the
// place: the directory line alone is the context, and a project with neither a
// manifest nor a subdirectory is still a project.
func TestSnapshotOfABareProjectIsNotEmpty(t *testing.T) {
	project := newTestProject(t)
	snapshot := Snapshot(project)
	if snapshot == "" {
		t.Fatal("an empty project produced no snapshot")
	}
	if !strings.Contains(snapshot, "Project directory: ") {
		t.Fatalf("snapshot = %q", snapshot)
	}
	if strings.Contains(snapshot, "Language and build system") || strings.Contains(snapshot, "Directory tree") {
		t.Fatalf("a project with no markers or directories invented them: %q", snapshot)
	}
}

func TestSnapshotRefusesWhatIsNotAProject(t *testing.T) {
	if snapshot := Snapshot(""); snapshot != "" {
		t.Fatalf("empty root = %q", snapshot)
	}
	if snapshot := Snapshot(filepath.Join(t.TempDir(), "absent")); snapshot != "" {
		t.Fatalf("absent root = %q", snapshot)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if snapshot := Snapshot(file); snapshot != "" {
		t.Fatalf("a file is not a project: %q", snapshot)
	}
}

// The snapshot is generated once and kept for the conversation's whole life, so
// two generations over an unchanged project are the same bytes. A snapshot that
// differed between turns would sit in front of the history and break the prefix
// cache on every turn.
func TestSnapshotIsStableAcrossCalls(t *testing.T) {
	project := newTestProject(t)
	writeProjectFile(t, project, "Cargo.toml", "[package]\nname = \"x\"\n")
	if err := os.MkdirAll(filepath.Join(project, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := Snapshot(project)
	second := Snapshot(project)
	if first != second {
		t.Fatalf("the snapshot changed between calls:\n%q\n%q", first, second)
	}
}

func TestSnapshotBoundsTheTree(t *testing.T) {
	project := newTestProject(t)
	// A directory wider than the per-directory limit, so the overflow is real.
	for index := 0; index < SnapshotEntriesPerDir+5; index++ {
		name := filepath.Join("wide", "dir-"+strconv.Itoa(index))
		if err := os.MkdirAll(filepath.Join(project, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := Snapshot(project)
	if !strings.Contains(snapshot, "(more directories not shown)") {
		t.Fatalf("a wide directory did not announce the overflow: %q", snapshot)
	}
	// And the depth limit: directories three levels deep are not listed.
	deep := filepath.Join(project, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, project, "package.json", `{}`)
	other := newTestProject(t)
	writeProjectFile(t, other, "package.json", `{}`)
	if err := os.MkdirAll(filepath.Join(other, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	otherSnapshot := Snapshot(other)
	if !strings.Contains(otherSnapshot, "- a/b/") || strings.Contains(otherSnapshot, "- a/b/c/") {
		t.Fatalf("the tree's depth is wrong: %q", otherSnapshot)
	}
}
