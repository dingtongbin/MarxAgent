// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPoolRefreshUsesProjectOverrideAndActivatesBody(t *testing.T) {
	globalRoot := t.TempDir()
	projectRoot := t.TempDir()
	writeSkill(t, globalRoot, "alpha", "global alpha", "global instructions\n")
	writeSkill(t, globalRoot, "shared", "global shared", "global body\n")
	writeSkill(t, projectRoot, "alpha", "project alpha", "project instructions\n")
	writeSkill(t, projectRoot, "beta", "project beta", "beta body\n")

	pool, err := NewPool(Config{GlobalDir: globalRoot, ProjectDir: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	index := pool.Index()
	if len(index) != 3 {
		t.Fatalf("index length = %d, want 3", len(index))
	}
	if index[0].Name != "alpha" || index[0].Source != SourceProject {
		t.Fatalf("first entry = %#v", index[0])
	}
	if index[1].Name != "beta" || index[2].Name != "shared" {
		t.Fatalf("index order = %#v", index)
	}
	index[0].Metadata = map[string]string{"mutated": "true"}
	if pool.Index()[0].Metadata != nil {
		t.Fatal("Index returned mutable metadata")
	}
	skill, err := pool.Activate(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if skill.Instructions != "project instructions\n" {
		t.Fatalf("instructions = %q", skill.Instructions)
	}
	if got := pool.IndexText(); got != "alpha: project alpha\nbeta: project beta\nshared: global shared\n" {
		t.Fatalf("index text = %q", got)
	}
	writeSkill(t, projectRoot, "new-skill", "new skill", "new body\n")
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := pool.Index(); len(got) != 4 || got[3].Name != "new-skill" {
		t.Fatalf("append-only index = %#v", got)
	}
}

func TestPoolParsesCRLFAndOptionalFrontmatter(t *testing.T) {
	root := t.TempDir()
	skillRoot := filepath.Join(root, "pdf")
	if err := os.MkdirAll(filepath.Join(skillRoot, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(skillRoot, "SKILL.md")
	content := "---\r\nname: pdf\r\ndescription: Extract PDF text when processing documents.\r\nkeywords:\r\n  - pdf\r\n  - extract\r\nlicense: Apache-2.0\r\ncompatibility: Requires a PDF reader\r\nmetadata:\r\n  author: test\r\nallowed-tools: read grep\r\n---\r\nUse the reference when needed.\r\n"
	if err := os.WriteFile(skillFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "references", "guide.md"), []byte("guide"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(Config{ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	skill, err := pool.Activate(context.Background(), "pdf")
	if err != nil {
		t.Fatal(err)
	}
	if skill.License != "Apache-2.0" || skill.Metadata["author"] != "test" || skill.AllowedTools != "read grep" || len(skill.Keywords) != 2 {
		t.Fatalf("metadata = %#v", skill.Entry)
	}
	if skill.Instructions != "Use the reference when needed.\r\n" {
		t.Fatalf("instructions = %q", skill.Instructions)
	}
	resource, err := pool.ReadResource(context.Background(), "pdf", "references/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(resource) != "guide" {
		t.Fatalf("resource = %q", resource)
	}
}

func TestPoolRejectsEscapingResources(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "safe", "safe skill", "body\n")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "safe", "references")
	if err := os.Symlink(outside, link); err == nil {
		defer os.Remove(link)
	}
	pool, err := NewPool(Config{ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ReadResource(context.Background(), "safe", "../outside.txt"); !errors.Is(err, ErrResourceOutside) {
		t.Fatalf("escape error = %v", err)
	}
	if _, err := pool.ReadResource(context.Background(), "safe", "references/missing.txt"); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("missing error = %v", err)
	}
}

func TestPoolSearchAndValidation(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "go-review", "Review Go code and tests.", "body\n")
	writeSkill(t, root, "python", "Write Python scripts.", "body\n")
	pool, err := NewPool(Config{ProjectDir: root, MaxResults: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, err := pool.Search(context.Background(), "review Go", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Name != "go-review" {
		t.Fatalf("results = %#v", results)
	}
	if _, err := pool.Search(context.Background(), " ", 1); err == nil {
		t.Fatal("empty query accepted")
	}
	badRoot := t.TempDir()
	badSkill := filepath.Join(badRoot, "bad")
	if err := os.MkdirAll(badSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badSkill, "SKILL.md"), []byte("---\nname: invalid\ndescription: bad\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	badPool, err := NewPool(Config{ProjectDir: badRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := badPool.Refresh(context.Background()); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("invalid skill error = %v", err)
	}
}

func TestPoolHonorsContextAndSizeLimits(t *testing.T) {
	if _, err := NewPool(Config{MaxFileBytes: -1}); err == nil {
		t.Fatal("negative max file bytes accepted")
	}
	root := t.TempDir()
	writeSkill(t, root, "large", "large skill", strings.Repeat("x", 32))
	pool, err := NewPool(Config{ProjectDir: root, MaxFileBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pool.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh error = %v", err)
	}
	if err := pool.Refresh(context.Background()); err == nil {
		t.Fatal("oversized skill accepted")
	}
}

func writeSkill(t *testing.T, root, name, description, body string) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
