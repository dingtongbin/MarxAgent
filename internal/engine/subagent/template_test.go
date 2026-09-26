// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplate puts one template on disk the way a person would.
func writeTemplate(t *testing.T, root, name, content string) string {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, TemplateFileName)
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func newTemplatePool(t *testing.T, globalDir, projectDir string) *TemplatePool {
	t.Helper()
	pool, err := NewTemplatePool(TemplateConfig{GlobalDir: globalDir, ProjectDir: projectDir})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// withTemplates writes templates and returns a pool that has already seen them.
//
// A pool that is refreshed by hand after every write is a pool where a forgotten
// refresh shows up as a template that does not exist, which is the shape of a bug
// that is really a test that forgot something.
func withTemplates(t *testing.T, globalDir, projectDir string,
	write func(global, project string)) *TemplatePool {
	t.Helper()
	write(globalDir, projectDir)
	pool := newTemplatePool(t, globalDir, projectDir)
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pool
}

const reviewerTemplate = `---
name: reviewer
description: Reviews a change and reports what is wrong with it
tools:
  - read
  - grep
max_iterations: 12
---

You review changes. Report a problem only when you can point at the line that has
it, and say what would be wrong rather than what you would have done.
`

func TestATemplateIsAnIndexEntryUntilItIsWanted(t *testing.T) {
	ctx := context.Background()
	pool := withTemplates(t, t.TempDir(), t.TempDir(), func(global, project string) {
		writeTemplate(t, project, "reviewer", reviewerTemplate)
	})
	entry, ok := pool.Lookup("reviewer")
	if !ok {
		t.Fatal("the template was not found after a refresh")
	}
	if entry.Description == "" {
		t.Fatal("the index entry has no description, which is what a main core reads to choose it")
	}
	if len(entry.Tools) != 2 || entry.Tools[0] != "read" {
		t.Fatalf("tools = %v", entry.Tools)
	}
	if entry.MaxIterations != 12 {
		t.Fatalf("max_iterations = %d", entry.MaxIterations)
	}
	// The index is what goes in a prompt, and the instructions are not in it.
	index := pool.IndexText()
	if !strings.Contains(index, "reviewer") || !strings.Contains(index, "Reviews a change") {
		t.Fatalf("index text = %q", index)
	}
	if strings.Contains(index, "point at the line") {
		t.Fatal("the index carries the instructions, which is the cost the index exists to avoid")
	}
	loaded, err := pool.Load(ctx, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instructions, "point at the line") {
		t.Fatalf("instructions = %q", loaded.Instructions)
	}
	// The entry and the loaded template agree, because they are two views of one
	// file and a caller that sees them disagree has to choose between them.
	if loaded.Name != entry.Name || loaded.MaxIterations != entry.MaxIterations {
		t.Fatalf("the entry %+v and the template %+v disagree", entry, loaded.TemplateEntry)
	}
}

func TestAProjectTemplateShadowsAGlobalOne(t *testing.T) {
	ctx := context.Background()
	globalDir, projectDir := t.TempDir(), t.TempDir()
	pool := withTemplates(t, globalDir, projectDir, func(global, project string) {
		writeTemplate(t, global, "reviewer", reviewerTemplate)
		writeTemplate(t, project, "reviewer", `---
name: reviewer
description: The project's own reviewer
tools: []
---

This project reviews differently.
`)
	})
	entry, ok := pool.Lookup("reviewer")
	if !ok {
		t.Fatal("the template was not found")
	}
	if entry.Source != SourceProject {
		t.Fatalf("source = %q, want the project's", entry.Source)
	}
	if entry.Description != "The project's own reviewer" {
		t.Fatalf("description = %q", entry.Description)
	}
	loaded, err := pool.Load(ctx, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instructions, "differently") {
		t.Fatalf("instructions = %q", loaded.Instructions)
	}
	// Shadowing replaces, it does not merge: a project narrowing a tool set means
	// fewer tools, and a union would quietly hand back what the project removed.
	if len(entry.Tools) != 0 {
		t.Fatalf("tools = %v, want none", entry.Tools)
	}
}

// A template is a file a person can point anywhere, and a main core that can be
// talked into reading a path is a main core that can be talked into reading anything.
func TestATemplateCannotBeReadFromOutsideItsOwnDirectory(t *testing.T) {
	ctx := context.Background()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("---\nname: reviewer\ndescription: outside\n---\nnot yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	directory := filepath.Join(root, "reviewer")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	// The file the pool finds is a link, and the link points outside. The scan
	// resolves the file to compare it against the root, so a link is followed here
	// and refused there rather than trusted.
	if err := os.Symlink(secret, filepath.Join(directory, TemplateFileName)); err != nil {
		t.Skipf("this host cannot make a symbolic link: %v", err)
	}
	pool := newTemplatePool(t, root, "")
	if entry, ok := pool.Lookup("reviewer"); ok {
		t.Fatalf("a template reached through a link outside its directory was indexed: %+v", entry)
	}
	if _, err := pool.Load(ctx, "reviewer"); err == nil {
		t.Fatal("a template was read from outside its own directory")
	}
}

// A directory without a template is a directory holding something else, not a
// broken template. A refresh that failed on one would make the whole pool unusable
// because of a folder nobody meant to put a sub agent in.
func TestADirectoryWithoutATemplateIsSkipped(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	pool := withTemplates(t, root, "", func(global, project string) {
		writeTemplate(t, root, "reviewer", reviewerTemplate)
	})
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("stray"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if len(pool.Index()) != 1 {
		t.Fatalf("index = %d entries, want one", len(pool.Index()))
	}
}

// The pool is useless if a broken template cannot be reported, so these are the
// ways a file can be wrong and each one has to be caught with a message that names
// the file.
func TestABrokenTemplateIsRefusedWithAReason(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{name: "no header", content: "just prose", want: "must start with"},
		{name: "unclosed header", content: "---\nname: x\n", want: "not closed"},
		{
			name:    "a name that disagrees with the directory",
			content: "---\nname: other\ndescription: disagrees\n---\nbody\n",
			want:    "disagree",
		},
		{name: "no description", content: "---\nname: x\n---\nbody\n", want: "description"},
		{
			name:    "no instructions",
			content: "---\nname: x\ndescription: has none\n---\n\n\n",
			want:    "no instructions",
		},
		{
			name:    "an upper case name",
			content: "---\nname: Reviewer\ndescription: upper case\n---\nbody\n",
			want:    "not portable",
		},
		{
			name:    "a name with two dashes",
			content: "---\nname: a--b\ndescription: two dashes\n---\nbody\n",
			want:    "two dashes",
		},
		{
			name:    "a name with a slash",
			content: "---\nname: a/b\ndescription: a slash\n---\nbody\n",
			want:    "contains",
		},
		{
			name:    "a negative iteration count",
			content: "---\nname: x\ndescription: negative\nmax_iterations: -1\n---\nbody\n",
			want:    "not a count",
		},
		{
			name:    "an iteration count over the limit",
			content: "---\nname: x\ndescription: too many\nmax_iterations: 1000\n---\nbody\n",
			want:    "over the",
		},
		{
			name:    "a blank tool",
			content: "---\nname: x\ndescription: blank tool\ntools:\n  - \"  \"\n---\nbody\n",
			want:    "blank entry",
		},
		{
			name:    "a header that is not a mapping",
			content: "---\n- a\n- b\n---\nbody\n",
			want:    "header",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			pool := newTemplatePool(t, root, "")
			writeTemplate(t, root, "x", test.content)
			err := pool.Refresh(context.Background())
			if err == nil {
				_, loadErr := pool.Load(context.Background(), "x")
				if loadErr == nil {
					t.Fatal("a broken template was accepted")
				}
				err = loadErr
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestTemplateSearchFindsByNameDescriptionAndKeyword(t *testing.T) {
	ctx := context.Background()
	pool := withTemplates(t, t.TempDir(), t.TempDir(), func(global, project string) {
		writeTemplate(t, project, "reviewer", reviewerTemplate)
		writeTemplate(t, project, "summariser", "---\nname: summariser\ndescription: Condenses a long document\nkeywords:\n  - condense\n  - shorten\n---\n\nYou condense.\n")
	})
	for _, query := range []string{"review", "Reviews a change", "condense", "shorten", "SUMMARISER"} {
		found, err := pool.Search(ctx, query, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(found) == 0 {
			t.Fatalf("the search for %q found nothing", query)
		}
	}
	if found, err := pool.Search(ctx, "nothing here at all", 10); err != nil || len(found) != 0 {
		t.Fatalf("found = %v, err = %v", found, err)
	}
	if _, err := pool.Search(ctx, "  ", 10); err == nil {
		t.Fatal("an empty search was accepted")
	}
	if _, err := pool.Load(ctx, "absent"); err == nil {
		t.Fatal("a missing template loaded")
	}
}

// A refresh that reorders the index invalidates the prefix in front of the model
// for no reason, so an index a caller has already read keeps its order.
func TestARefreshKeepsTheOrderACallerHasSeen(t *testing.T) {
	ctx := context.Background()
	projectDir := t.TempDir()
	pool := withTemplates(t, "", projectDir, func(global, project string) {
		writeTemplate(t, project, "alpha", "---\nname: alpha\ndescription: first\n---\na\n")
		writeTemplate(t, project, "beta", "---\nname: beta\ndescription: second\n---\nb\n")
	})
	before := pool.IndexText()
	if !strings.HasPrefix(before, "alpha") {
		t.Fatalf("index = %q", before)
	}
	// A third template sorts first alphabetically and would move the other two.
	writeTemplate(t, projectDir, "aardvark", `---
name: aardvark
description: zeroth
---
z`)
	if err := pool.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	after := pool.IndexText()
	if !strings.HasPrefix(after, "alpha") {
		t.Fatalf("a refresh moved an index a caller had already read: %q", after)
	}
	if !strings.Contains(after, "aardvark") {
		t.Fatal("the new template was not added")
	}
}

func TestTheTemplatePoolRefusesNonsenseConfiguration(t *testing.T) {
	if _, err := NewTemplatePool(TemplateConfig{MaxFileBytes: -1}); err == nil {
		t.Fatal("a negative file limit was accepted")
	}
	if _, err := NewTemplatePool(TemplateConfig{MaxResults: -1}); err == nil {
		t.Fatal("a negative result limit was accepted")
	}
	pool, err := NewTemplatePool(TemplateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// A pool with no directories is a machine with no sub agents, which is normal.
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pool.Index()) != 0 {
		t.Fatal("an empty pool has entries")
	}
	if _, err := pool.Load(nil, "x"); err == nil { //nolint:staticcheck // a nil context is the mistake under test
		t.Fatal("a missing context was accepted")
	}
}
