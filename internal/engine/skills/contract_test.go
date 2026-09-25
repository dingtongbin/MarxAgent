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

func TestNewPoolRejectsInvalidDirectoriesAndLimits(t *testing.T) {
	if _, err := NewPool(Config{GlobalDir: "bad\x00dir"}); err == nil {
		t.Fatal("NUL global directory accepted")
	}
	if _, err := NewPool(Config{ProjectDir: "bad\x00dir"}); err == nil {
		t.Fatal("NUL project directory accepted")
	}
	if _, err := NewPool(Config{MaxResults: -1}); err == nil {
		t.Fatal("negative max results accepted")
	}
	pool, err := NewPool(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if pool.maxBytes != defaultMaxFileBytes || pool.maxResults != defaultMaxResults {
		t.Fatalf("defaults = %d %d", pool.maxBytes, pool.maxResults)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pool.Index()) != 0 {
		t.Fatal("empty pool returned entries")
	}
}

func TestPoolRejectsNilAndCanceledContexts(t *testing.T) {
	pool, err := NewPool(Config{ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pool.Refresh(nil); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil refresh context = %v", err)
	}
	if _, err := pool.Search(nil, "x", 1); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil search context = %v", err)
	}
	if _, err := pool.Search(ctx, "x", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled search = %v", err)
	}
	if _, err := pool.Activate(nil, "x"); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil activate context = %v", err)
	}
	if _, err := pool.Activate(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled activate = %v", err)
	}
	if _, err := pool.ReadResource(nil, "x", "y"); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil resource context = %v", err)
	}
	if _, err := pool.ReadResource(ctx, "x", "y"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resource read = %v", err)
	}
}

func TestPoolRejectsUnusableRoots(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(Config{ProjectDir: file})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err == nil {
		t.Fatal("file root accepted as a skills directory")
	}
	missing, err := NewPool(Config{GlobalDir: filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Refresh(context.Background()); err != nil {
		t.Fatalf("missing root error = %v", err)
	}
	if len(missing.Index()) != 0 {
		t.Fatal("missing root produced entries")
	}
}

func TestPoolSkipsNonDirectoryAndSymlinkEntries(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "real", "real skill", "body\n")
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("stray file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeSkill(t, outside, "linked", "linked skill", "body\n")
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err == nil {
		defer os.Remove(filepath.Join(root, "linked"))
	}
	pool, err := NewPool(Config{ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	names := pool.Index()
	if len(names) != 1 || names[0].Name != "real" {
		t.Fatalf("index = %#v", names)
	}
}

func TestActivateRejectsInvalidNamesAndMissingSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "alpha", "alpha skill", "body\n")
	pool, err := NewPool(Config{ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Activate(context.Background(), "   "); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("blank name = %v", err)
	}
	if _, err := pool.Activate(context.Background(), "missing"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("missing skill = %v", err)
	}
	writeSkill(t, root, "renamed", "renamed skill", "body\n")
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "renamed", "SKILL.md"), []byte("---\nname: renamed\ndescription: renamed skill\n---\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Activate(context.Background(), "renamed"); err != nil {
		t.Fatalf("valid rename = %v", err)
	}
	directory := filepath.Join(root, "mismatch")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: other\ndescription: mismatch\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	badPool, err := NewPool(Config{ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := badPool.Refresh(context.Background()); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("directory mismatch error = %v", err)
	}
}

func TestFrontmatterValidationRules(t *testing.T) {
	longName := strings.Repeat("a", 65)
	longDescription := strings.Repeat("d", 1025)
	longCompatibility := strings.Repeat("c", 501)
	manyKeywords := make([]string, 33)
	for index := range manyKeywords {
		manyKeywords[index] = "keyword"
	}
	cases := []struct {
		name  string
		front frontmatter
	}{
		{name: "empty name", front: frontmatter{Description: "ok"}},
		{name: "long name", front: frontmatter{Name: longName, Description: "ok"}},
		{name: "leading hyphen", front: frontmatter{Name: "-lead", Description: "ok"}},
		{name: "trailing hyphen", front: frontmatter{Name: "trail-", Description: "ok"}},
		{name: "double hyphen", front: frontmatter{Name: "a--b", Description: "ok"}},
		{name: "invalid character", front: frontmatter{Name: "Upper_Case", Description: "ok"}},
		{name: "empty description", front: frontmatter{Name: "ok"}},
		{name: "long description", front: frontmatter{Name: "ok", Description: longDescription}},
		{name: "long compatibility", front: frontmatter{Name: "ok", Description: "ok", Compatibility: longCompatibility}},
		{name: "too many keywords", front: frontmatter{Name: "ok", Description: "ok", Keywords: manyKeywords}},
		{name: "blank keyword", front: frontmatter{Name: "ok", Description: "ok", Keywords: []string{"  "}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := validateFrontmatter(test.front); err == nil {
				t.Fatal("invalid frontmatter accepted")
			}
		})
	}
	valid := frontmatter{Name: "ok", Description: "ok", Keywords: []string{"a", "A", " a ", "b"}}
	if err := validateFrontmatter(valid); err != nil {
		t.Fatal(err)
	}
	if keywords := normalizeKeywords(valid.Keywords); len(keywords) != 2 || keywords[0] != "a" || keywords[1] != "b" {
		t.Fatalf("keywords = %#v", keywords)
	}
	if err := yamlUnmarshal([]byte("name: ok\n"), nil); err == nil {
		t.Fatal("nil frontmatter output accepted")
	}
	if err := yamlUnmarshal([]byte("name: [unterminated\n"), &frontmatter{}); err == nil {
		t.Fatal("malformed YAML accepted")
	}
}

func TestFrontmatterSplitting(t *testing.T) {
	if _, _, err := splitFrontmatter([]byte("no frontmatter\n")); err == nil {
		t.Fatal("missing frontmatter accepted")
	}
	if _, _, err := splitFrontmatter([]byte("---\nname: unclosed\n")); err == nil {
		t.Fatal("unclosed frontmatter accepted")
	}
	if _, _, err := splitFrontmatter([]byte("\xef\xbb\xbf---\r\nname: bom\r\n---\r\nbody\r\n")); err != nil {
		t.Fatalf("BOM frontmatter error = %v", err)
	}
	front, body, err := splitFrontmatter([]byte("\xef\xbb\xbf---\r\nname: bom\r\n---\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(front), "name: bom") || string(body) != "body\r\n" {
		t.Fatalf("front = %q body = %q", front, body)
	}
	if lineEnd([]byte("abc"), -1) != -1 || lineEnd(nil, 0) != -1 || lineEnd([]byte("abc"), 0) != 3 {
		t.Fatal("lineEnd is incorrect")
	}
}

func TestParseEntryRejectsUnknownSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sample")
	pool, err := NewPool(Config{})
	if err != nil {
		t.Fatal(err)
	}
	front := []byte("name: sample\ndescription: sample skill\n")
	if _, err := pool.parseEntry(root, filepath.Join(root, "SKILL.md"), front, Source("bogus")); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("unknown source = %v", err)
	}
	if _, err := pool.parseEntry(root, filepath.Join(root, "SKILL.md"), front, SourceGlobal); err != nil {
		t.Fatalf("global source = %v", err)
	}
	entry, err := pool.parseEntry(root, filepath.Join(root, "SKILL.md"), front, SourceProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(entry.Keywords) == 0 {
		t.Fatal("description keywords were not derived")
	}
	mismatched := filepath.Join(t.TempDir(), "other")
	if _, err := pool.parseEntry(mismatched, filepath.Join(mismatched, "SKILL.md"), front, SourceProject); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("directory mismatch = %v", err)
	}
}

func TestResourcePathRules(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resourcePath(root, "   "); !errors.Is(err, ErrResourceOutside) {
		t.Fatalf("blank resource = %v", err)
	}
	if _, err := resourcePath(root, "bad\x00path"); !errors.Is(err, ErrResourceOutside) {
		t.Fatalf("NUL resource = %v", err)
	}
	if _, err := resourcePath(root, filepath.Join(root, "file.txt")); !errors.Is(err, ErrResourceOutside) {
		t.Fatalf("absolute resource = %v", err)
	}
	if _, err := resourcePath(root, "."); !errors.Is(err, ErrResourceOutside) {
		t.Fatalf("dot resource = %v", err)
	}
	if _, err := resourcePath(root, ".."); !errors.Is(err, ErrResourceOutside) {
		t.Fatalf("parent resource = %v", err)
	}
	if _, err := resourcePath(root, "dir"); err == nil {
		t.Fatal("directory accepted as a resource")
	}
	if _, err := resourcePath(root, "missing.txt"); err != nil {
		t.Fatalf("missing resource should be reported on read: %v", err)
	}
}

func TestReadResourceRejectsUnknownSkill(t *testing.T) {
	pool, err := NewPool(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ReadResource(context.Background(), "missing", "file.txt"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("unknown skill = %v", err)
	}
}

func TestSearchRejectsQueriesWithoutTerms(t *testing.T) {
	pool, err := NewPool(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Search(context.Background(), "!!!", 1); err == nil {
		t.Fatal("query without terms accepted")
	}
	if results, err := pool.Search(context.Background(), "anything", 0); err != nil || len(results) != 0 {
		t.Fatalf("empty pool search = %#v err = %v", results, err)
	}
}

func TestSearchTruncatesAndOrdersTiesByName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "beta", "shared description", "body\n")
	writeSkill(t, root, "alpha", "shared description", "body\n")
	pool, err := NewPool(Config{ProjectDir: root, MaxResults: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	results, err := pool.Search(context.Background(), "shared", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Name != "alpha" {
		t.Fatalf("truncated results = %#v", results)
	}
	all, err := pool.Search(context.Background(), "shared", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name != "alpha" || all[1].Name != "beta" {
		t.Fatalf("tie order = %#v", all)
	}
	if none, err := pool.Search(context.Background(), "absent", 8); err != nil || len(none) != 0 {
		t.Fatalf("unmatched search = %#v err = %v", none, err)
	}
}

func TestActivateRejectsSymlinkedSkillFiles(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "alpha", "alpha skill", "body\n")
	pool, err := NewPool(Config{ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(outside, []byte("---\nname: alpha\ndescription: alpha skill\n---\nbody\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alpha", "SKILL.md")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := pool.Activate(context.Background(), "alpha"); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("symlinked skill file = %v", err)
	}
}

func TestRefreshRejectsSkillWithoutFrontmatter(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "plain")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("just prose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(Config{ProjectDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Refresh(context.Background()); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("missing frontmatter = %v", err)
	}
}

func TestNextOrderIsAppendOnlyAndDeduplicated(t *testing.T) {
	entries := map[string]Entry{"b": {}, "a": {}}
	order := nextOrder([]string{"b", "b", "gone"}, entries)
	if len(order) != 2 || order[0] != "b" || order[1] != "a" {
		t.Fatalf("order = %#v", order)
	}
	if len(nextOrder(nil, entries)) != 2 {
		t.Fatal("order from scratch is incorrect")
	}
}

func TestNormalizeRootRules(t *testing.T) {
	root, err := normalizeRoot("  ")
	if err != nil || root != "" {
		t.Fatalf("blank root = %q err = %v", root, err)
	}
	if _, err := normalizeRoot("bad\x00root"); err == nil {
		t.Fatal("NUL root accepted")
	}
	absolute, err := normalizeRoot("relative")
	if err != nil || !filepath.IsAbs(absolute) {
		t.Fatalf("relative root = %q err = %v", absolute, err)
	}
}

func TestReadFileLimitedEnforcesLimits(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "data.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileLimited(context.Background(), path, 4); err == nil {
		t.Fatal("oversized file accepted")
	}
	data, err := readFileLimited(context.Background(), path, maxInt64)
	if err != nil || string(data) != "0123456789" {
		t.Fatalf("unbounded read = %q err = %v", data, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readFileLimited(ctx, path, 16); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %v", err)
	}
}
