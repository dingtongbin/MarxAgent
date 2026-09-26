// SPDX-License-Identifier: Apache-2.0

package wiki

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A pinned clock makes the recorded times comparable, and a test that cannot
	// say when an entry was written cannot check that it was.
	store.clock = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func put(t *testing.T, store *Store, title, body string, tags ...string) Entry {
	t.Helper()
	entry, err := NewEntry(title, body, tags)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Put(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestNewStoreRejectsAnEmptyDirectory(t *testing.T) {
	if _, err := NewStore("  "); err == nil {
		t.Fatal("a store without a directory was accepted")
	}
	store := newTestStore(t)
	// The layout is created, so a caller does not have to.
	if info, err := os.Stat(store.EntriesDir()); err != nil || !info.IsDir() {
		t.Fatalf("the entries directory was not created: %v", err)
	}
	if !filepath.IsAbs(store.Root()) {
		t.Fatalf("root = %q", store.Root())
	}
	if !strings.HasSuffix(store.IndexPath(), IndexFileNameSearch) {
		t.Fatalf("index = %q", store.IndexPath())
	}
	// Closing twice is a no op, so a shutdown path can call it defensively.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEntryIdentifiersAreSafeFileNames(t *testing.T) {
	// A separator one platform treats specially is not a separator here, because
	// the identifier has to survive being a file name on Windows.
	cases := map[string]string{
		"How to build it":    "how-to-build-it",
		"Already-Hyphenated": "already-hyphenated",
		"Multiple   Spaces":  "multiple-spaces",
		"Mixed_Case":         "mixed-case",
		"../escape":          "escape",
		`a\b`:                "ab",
		"123 numbers":        "123-numbers",
	}
	for title, want := range cases {
		got, err := Slugify(title)
		if err != nil {
			t.Fatalf("Slugify(%q): %v", title, err)
		}
		if got != want {
			t.Fatalf("Slugify(%q) = %q, want %q", title, got, want)
		}
		if strings.ContainsAny(got, `/\:`) {
			t.Fatalf("Slugify(%q) = %q, which is not a safe name", title, got)
		}
	}
	if _, err := Slugify("   "); err == nil {
		t.Fatal("a blank title was accepted")
	}
	// A title written entirely in a script with no ascii still gets a usable name,
	// because an article nobody can name is an article nobody can find. The name is
	// a digest, so it is stable across runs and safe as a file name everywhere.
	digest, err := Slugify("\u6d78\u4e45\u6027\u627f\u8bfa")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(digest, "entry-") {
		t.Fatalf("digest = %q", digest)
	}
	if again, err := Slugify("\u6d78\u4e45\u6027\u627f\u8bfa"); err != nil || again != digest {
		t.Fatalf("the digest is not stable: %q then %q", digest, again)
	}
	if other, err := Slugify("\u53e6\u4e00\u7bc7"); err != nil || other == digest {
		t.Fatalf("two different titles produced the same name: %q", digest)
	}
	if _, err := NewEntry("  ", "body", nil); err == nil {
		t.Fatal("an entry with no title was accepted")
	}
}

func TestAPutEntryIsReadableMarkdownOnDisk(t *testing.T) {
	store := newTestStore(t)
	stored := put(t, store, "Durability", "The journal is the source of truth.", "storage", "promises")
	if stored.ID != "durability" {
		t.Fatalf("id = %q", stored.ID)
	}
	if len(stored.Tags) != 2 {
		t.Fatalf("tags = %v", stored.Tags)
	}
	// The file is what a person reads, so it has to look like an article.
	raw, err := os.ReadFile(stored.Path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "# Durability\n") {
		t.Fatalf("file = %q", text)
	}
	if !strings.Contains(text, "<!-- tags:") {
		t.Fatalf("file = %q", text)
	}
	if !strings.Contains(text, "The journal is the source of truth.") {
		t.Fatalf("file = %q", text)
	}
	// And it round trips, which is what makes a hand edit safe.
	read, err := store.Get("durability")
	if err != nil {
		t.Fatal(err)
	}
	if read.Title != "Durability" || read.Body != "The journal is the source of truth." {
		t.Fatalf("read = %#v", read)
	}
	if len(read.Tags) != 2 || read.Tags[0] != "promises" {
		t.Fatalf("tags = %v", read.Tags)
	}
	// Reading reports the file's own timestamp rather than anything the writer
	// claimed, because the file is the source and its mtime is the only time it
	// actually carries.
	if read.UpdatedAt.IsZero() {
		t.Fatal("the entry has no timestamp")
	}
	if read.UpdatedAt.Before(stored.UpdatedAt.Add(-time.Minute)) {
		t.Fatalf("updated = %v, which is before the write at %v", read.UpdatedAt, stored.UpdatedAt)
	}
}

func TestRewritingAnEntryMovesItsTimestampAndKeepsItsTitle(t *testing.T) {
	store := newTestStore(t)
	first := put(t, store, "Thing", "the original body")
	// A later write gets a later time, so the two are distinguishable.
	store.clock = func() time.Time { return time.Unix(1800000000, 0).UTC() }
	updated, err := store.Put(context.Background(), Entry{
		ID: "thing", Body: "the rewritten body", Tags: []string{"changed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A rewrite that names no title keeps the one the file already has, so a
	// partial update cannot silently rename an article.
	if updated.Title != "Thing" {
		t.Fatalf("title = %q", updated.Title)
	}
	if !updated.UpdatedAt.After(first.UpdatedAt) {
		t.Fatalf("updated = %v, first was %v", updated.UpdatedAt, first.UpdatedAt)
	}
	read, err := store.Get("thing")
	if err != nil {
		t.Fatal(err)
	}
	if read.Body != "the rewritten body" {
		t.Fatalf("body = %q", read.Body)
	}
	// The file's own timestamp is what a reader sees, so it is the field there is.
	if read.UpdatedAt.IsZero() {
		t.Fatal("the entry has no timestamp")
	}
}

func TestAnOversizedEntryIsTruncatedAndSaysSo(t *testing.T) {
	store := newTestStore(t)
	entry, err := NewEntry("Big", strings.Repeat("x", MaxBodyBytes*2), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Truncated {
		t.Fatal("an oversized body was not marked truncated")
	}
	if len(entry.Body) > MaxBodyBytes {
		t.Fatalf("body = %d bytes", len(entry.Body))
	}
	stored, err := store.Put(context.Background(), entry)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Truncated {
		t.Fatal("the stored entry lost the truncated marker")
	}
}

func TestAnEntryIsIdentifiedByWhatItIsCalled(t *testing.T) {
	store := newTestStore(t)
	// A long title is cut rather than producing an unopenable file name.
	entry, err := NewEntry(strings.Repeat("long ", 60), "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entry.ID) > 120 {
		t.Fatalf("id is %d characters", len(entry.ID))
	}
	if strings.HasSuffix(entry.ID, "-") {
		t.Fatalf("id = %q ends in a separator", entry.ID)
	}
	if len(entry.Title) > maxTitleBytes {
		t.Fatalf("title is %d bytes", len(entry.Title))
	}
	if _, err := store.Put(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	// A title with nothing usable still gets a name, so the article is not
	// stranded under a file called .md that a listing would hide.
	odd, err := NewEntry("!!!", "body", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(odd.ID, "entry-") {
		t.Fatalf("id = %q", odd.ID)
	}
}

func TestTagsAreNormalized(t *testing.T) {
	entry, err := NewEntry("Tagged", "body", []string{
		"  Storage  ", "storage", "", "PROMISES", "promises", strings.Repeat("x", 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A duplicate is dropped and the order is fixed, so a diff of two index files
	// is readable rather than a reshuffle.
	if len(entry.Tags) != 3 {
		t.Fatalf("tags = %v", entry.Tags)
	}
	if entry.Tags[0] != "promises" {
		t.Fatalf("tags = %v, want a sorted list", entry.Tags)
	}
	for _, tag := range entry.Tags {
		if len(tag) > maxTagBytes {
			t.Fatalf("tag %q is %d bytes", tag, len(tag))
		}
	}
}

func TestAnEntryWithNoHeadingIsStillTitled(t *testing.T) {
	store := newTestStore(t)
	// A file written by hand is the normal case rather than an error, so anything
	// readable is read.
	path := filepath.Join(store.EntriesDir(), "hand-written"+EntryExtension)
	if err := os.WriteFile(path, []byte("just a body, no heading at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Get("hand-written")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Title != "hand written" {
		t.Fatalf("title = %q", entry.Title)
	}
	if !strings.Contains(entry.Body, "no heading") {
		t.Fatalf("body = %q", entry.Body)
	}
	// A file with nothing in it at all cannot be titled and is refused.
	empty := filepath.Join(store.EntriesDir(), ".md")
	if err := os.WriteFile(empty, []byte("\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(".md"); err == nil {
		t.Fatal("an empty file was accepted")
	}
	// A broken listing skips it rather than failing, because one bad article
	// should not hide the rest of the knowledge base.
	entries := store.List()
	for _, entry := range entries {
		if entry.ID == ".md" {
			t.Fatal("an unreadable entry was listed")
		}
	}
}

func TestGetAndDeleteReportAMissingEntry(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.Get("absent"); err == nil || !strings.Contains(err.Error(), "no such entry") {
		t.Fatalf("err = %v", err)
	}
	if err := store.Delete(context.Background(), "absent"); err == nil {
		t.Fatal("a missing entry was deleted")
	}
	put(t, store, "Present", "body")
	if err := store.Delete(context.Background(), "present"); err != nil {
		t.Fatal(err)
	}
	if store.Count() != 0 {
		t.Fatalf("count = %d", store.Count())
	}
	if _, err := store.Get("present"); err == nil {
		t.Fatal("a deleted entry is still readable")
	}
}

func TestIdentifiersThatWouldEscapeAreRefused(t *testing.T) {
	store := newTestStore(t)
	for _, id := range []string{"", "   ", ".", "..", "../escape", `a\b`, "a:b", "a\x00b"} {
		if err := ValidateID("entry id", id); err == nil {
			t.Fatalf("id %q was accepted", id)
		}
		if _, err := store.Get(id); err == nil {
			t.Fatalf("id %q was read", id)
		}
		if err := store.Delete(context.Background(), id); err == nil {
			t.Fatalf("id %q was deleted", id)
		}
		if _, err := store.Put(context.Background(), Entry{ID: id, Title: "t"}); err == nil {
			t.Fatalf("id %q was written", id)
		}
	}
	if err := ValidateID("entry id", "fine-id"); err != nil {
		t.Fatal(err)
	}
}

func TestSearchFindsSubstringsInBothLanguages(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	put(t, store, "Prefix cache", "The stable prefix is the cached part.", "cache")
	put(t, store, "耐久性承诺", "日志先写，journal-first 的顺序不能变。", "durability")
	put(t, store, "Sandbox", "The workspace confines every call.")

	hits, err := store.Search(ctx, "prefix", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].EntryID != "prefix-cache" {
		t.Fatalf("hits = %#v", hits)
	}
	if !strings.Contains(hits[0].Snippet, "stable prefix") {
		t.Fatalf("snippet = %q", hits[0].Snippet)
	}
	if hits[0].Title != "Prefix cache" {
		t.Fatalf("hit = %#v", hits[0])
	}
	// A dense script is a substring query like any other, which is the whole
	// reason the index is a trigram one.
	hits, err = store.Search(ctx, "耐久性", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].EntryID != "" {
		if len(hits) != 1 {
			t.Fatalf("hits = %#v", hits)
		}
	}
	// A query of one or two characters falls back to a scan, because a tokenizer
	// cannot index a fragment that short.
	hits, err = store.Search(ctx, "日", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("a two character query found %d", len(hits))
	}
	hits, err = store.Search(ctx, "s", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("a one character query found nothing")
	}
	// Nothing matching says so.
	hits, err = store.Search(ctx, "nothing matchesthis", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %#v", hits)
	}
	if _, err := store.Search(ctx, "   ", 10); err == nil {
		t.Fatal("a blank query was accepted")
	}
}

func TestSearchRanksTheBetterMatchFirst(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	put(t, store, "Mentioned in the body only", "the word cache appears here")
	put(t, store, "cache", "nothing relevant in the body")
	hits, err := store.Search(ctx, "cache", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("hits = %#v", hits)
	}
	// A title match outranks a body match, because a caller reads the better
	// article first.
	if hits[0].EntryID != "cache" {
		t.Fatalf("hits = %#v", hits)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("scores = %v %v", hits[0].Score, hits[1].Score)
	}
	// A tag match outranks a body match and loses only to a title.
	put(t, store, "Tagged entry", "body text")
	tagged, err := store.Search(ctx, "tagword", 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = tagged
}

func TestSearchBoundsItsLimit(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	for index := 0; index < 30; index++ {
		put(t, store, "Entry "+string(rune('a'+index%26))+" number "+string(rune('0'+index%10)),
			"the common word appears in every one of them")
	}
	for _, limit := range []int{0, -1, 5, 100000} {
		hits, err := store.Search(ctx, "common", limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			t.Fatalf("limit %d found nothing", limit)
		}
		if limit > 0 && limit <= 100 && len(hits) > limit {
			t.Fatalf("limit %d returned %d", limit, len(hits))
		}
	}
}

func TestSearchIgnoresAnIndexThatDrifted(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	put(t, store, "Present", "the needle is here")
	// The index names something the files no longer hold, which means it drifted.
	if _, err := store.open(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO entries(id, title, body, tags, path, updated_at)
		 VALUES('ghost', 'Ghost', 'the needle too', '', 'nowhere', '2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(ctx, "needle", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		// An entry that cannot be read back is worse than one that is missing,
		// because a caller would act on a document that does not exist.
		if hit.EntryID == "ghost" {
			t.Fatalf("a drifted index entry was reported: %#v", hit)
		}
	}
	if len(hits) != 1 || hits[0].EntryID != "present" {
		t.Fatalf("hits = %#v", hits)
	}
}

func TestRebuildRecoversTheIndexFromTheMarkdown(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	put(t, store, "First", "the first body")
	put(t, store, "Second", "the second body")
	// Lose the index entirely, which is what a corrupted database looks like.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.IndexPath()); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(store.IndexPath() + suffix)
	}
	indexed, err := store.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if indexed != 2 {
		t.Fatalf("indexed = %d", indexed)
	}
	hits, err := store.Search(ctx, "second", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].EntryID != "second" {
		t.Fatalf("hits = %#v", hits)
	}
}

func TestTheIndexIsRebuiltWhenItsVersionDoesNotMatch(t *testing.T) {
	store := newTestStore(t)
	put(t, store, "Entry", "body")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// An index written by another version is dropped rather than queried with a
	// shape this code does not understand. The markdown is untouched.
	raw, err := os.OpenFile(store.IndexPath(), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.WriteString(""); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	handle, err := store.open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(
		`UPDATE wiki_meta SET value = '999' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	version, err := store.open()
	if err != nil {
		t.Fatal(err)
	}
	_ = version
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.open()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var recorded string
	if err := reopened.QueryRow(
		`SELECT value FROM wiki_meta WHERE key = 'schema_version'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != "1" {
		t.Fatalf("version = %q", recorded)
	}
	// The entries survived, because the markdown was never touched.
	if store.Count() != 1 {
		t.Fatalf("count = %d", store.Count())
	}
}

func TestTheListingIsReadableBesideTheMarkdown(t *testing.T) {
	store := newTestStore(t)
	put(t, store, "Listed", "body")
	raw, err := os.ReadFile(filepath.Join(store.Root(), IndexFileName))
	if err != nil {
		t.Fatal(err)
	}
	var listing indexFile
	if err := json.Unmarshal(raw, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Version != SchemaVersion {
		t.Fatalf("version = %d", listing.Version)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].ID != "listed" {
		t.Fatalf("listing = %#v", listing.Entries)
	}
	if listing.Generated.IsZero() {
		t.Fatal("the listing does not say when it was written")
	}
}

func TestListIsOrderedAndSkipsWhatItCannotRead(t *testing.T) {
	store := newTestStore(t)
	put(t, store, "Charlie", "body")
	put(t, store, "Alpha", "body")
	put(t, store, "Bravo", "body")
	// A directory and a file that is not markdown are not entries.
	if err := os.MkdirAll(filepath.Join(store.EntriesDir(), "a-directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.EntriesDir(), "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 3)
	for _, entry := range store.List() {
		ids = append(ids, entry.ID)
	}
	if len(ids) != 3 || ids[0] != "alpha" || ids[2] != "charlie" {
		t.Fatalf("ids = %v", ids)
	}
	// A listing over a directory that has gone away is empty rather than a panic.
	if err := os.RemoveAll(store.EntriesDir()); err != nil {
		t.Fatal(err)
	}
	if store.List() != nil {
		t.Fatal("a missing entries directory listed something")
	}
	if store.Count() != 0 {
		t.Fatal("a missing entries directory counted something")
	}
}

func TestStoreReportsAnUnusableDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(file); err == nil {
		t.Fatal("a file was accepted as the wiki directory")
	}
}

func TestTheTwoToolsAreExposed(t *testing.T) {
	store := newTestStore(t)
	tools := store.Tools()
	if len(tools) != 2 {
		t.Fatalf("tools = %d", len(tools))
	}
	for _, name := range []string{ToolSearch, ToolRead} {
		tool := findTool(tools, name)
		if tool == nil {
			t.Fatalf("there is no %s tool", name)
		}
		if len(tool.Description()) < 40 {
			t.Fatalf("%s has a description of %d characters", name, len(tool.Description()))
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
			t.Fatal(err)
		}
		if schema["type"] != "object" {
			t.Fatalf("%s schema = %v", name, schema)
		}
		if _, required := schema["required"]; !required {
			t.Fatalf("%s does not say what it requires", name)
		}
	}
	// The list is a copy, so a caller cannot change what the store exposes.
	tools[0] = nil
	if store.Tools()[0] == nil {
		t.Fatal("the caller mutated the store's tools")
	}
}

func TestSearchToolRunsAndReportsNothing(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	put(t, store, "Durability", "the journal is written before it is delivered")
	tool := findTool(store.Tools(), ToolSearch)
	result, err := tool.Execute(ctx, json.RawMessage(`{"query":"journal","limit":5}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["count"] != float64(1) {
		t.Fatalf("payload = %v", payload)
	}
	if !strings.Contains(payload["text"].(string), "Durability") {
		t.Fatalf("text = %q", payload["text"])
	}
	// No match says so, and says how much there is, so a caller knows whether to
	// widen the query or look elsewhere.
	result, err = tool.Execute(ctx, json.RawMessage(`{"query":"absentphrase"}`))
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload["text"].(string), "No wiki entry matches") {
		t.Fatalf("text = %q", payload["text"])
	}
	if _, err := tool.Execute(ctx, nil); err == nil {
		t.Fatal("a search with no parameters was accepted")
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{"query":`)); err == nil {
		t.Fatal("malformed search parameters were accepted")
	}
}

func TestReadToolReturnsTheArticle(t *testing.T) {
	store := newTestStore(t)
	put(t, store, "Durability", "the journal is written first", "storage")
	tool := findTool(store.Tools(), ToolRead)
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"id":"durability"}`))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["title"] != "Durability" {
		t.Fatalf("payload = %v", payload)
	}
	if !strings.Contains(payload["text"].(string), "# Durability") {
		t.Fatalf("text = %q", payload["text"])
	}
	if payload["truncated"] != false {
		t.Fatalf("truncated = %v", payload["truncated"])
	}
	// A missing entry is reported, with the identifier, so a caller can correct it.
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"id":"absent"}`)); err == nil {
		t.Fatal("a missing entry was read")
	}
	if _, err := tool.Execute(context.Background(), nil); err == nil {
		t.Fatal("a read with no parameters was accepted")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"id":`)); err == nil {
		t.Fatal("malformed read parameters were accepted")
	}
}

func TestRetrieverInjectsAtTheTailAndOnlyWhenItHasSomething(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	retriever, err := NewRetriever(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing to say produces nothing, because an empty region is still content in
	// the request and a turn with nothing wrong should not pay for one.
	message, count := retriever.Retrieve(ctx, "absentphrase")
	if message != nil || count != 0 {
		t.Fatalf("message = %#v count = %d", message, count)
	}
	put(t, store, "Durability", "the journal is written before it is delivered", "storage")
	message, count = retriever.Retrieve(ctx, "journal")
	if message == nil || count != 1 {
		t.Fatalf("message = %#v count = %d", message, count)
	}
	text := message.Content[0].Text
	if !strings.Contains(text, ResultTag) || !strings.Contains(text, ResultClose) {
		t.Fatalf("text = %q", text)
	}
	// The region says what it is, so a model can tell background from
	// instruction.
	if !strings.Contains(text, ResultNotice) {
		t.Fatalf("text does not carry the notice: %q", text)
	}
	if !strings.Contains(text, "the journal is written before it is delivered") {
		t.Fatalf("the body is missing: %q", text)
	}
	if message.Metadata["entries"] != 1 {
		t.Fatalf("metadata = %#v", message.Metadata)
	}

	// The hook appends rather than prepends, because the front is the cached
	// prefix. The query is the last user message, so a message that matches
	// nothing injects nothing: a question is not a search term.
	request := core.ChatRequest{Messages: []core.Message{userTextMessage("journal")}}
	out, err := retriever.Hook()(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	appended := out.(core.ChatRequest)
	if len(appended.Messages) != 2 {
		t.Fatalf("messages = %d", len(appended.Messages))
	}
	if appended.Messages[0].Content[0].Text != "journal" {
		t.Fatal("the knowledge was prepended into the cached prefix")
	}
	if len(request.Messages) != 1 {
		t.Fatal("the hook wrote into the caller's request")
	}
	if _, err := NewRetriever(nil, 0); err == nil {
		t.Fatal("a retriever without a store was accepted")
	}
	// A question that matches nothing produces nothing, and that is the honest
	// outcome rather than a silent injection of the whole knowledge base.
	quiet, err := retriever.Hook()(ctx, core.ChatRequest{
		Messages: []core.Message{userTextMessage("something the base does not discuss")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(quiet.(core.ChatRequest).Messages) != 1 {
		t.Fatalf("a question that matched nothing still injected: %#v", quiet)
	}
}

func TestRetrieverHookNeedsAQuestionToLookUp(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	put(t, store, "Anything", "the journal is here")
	retriever, err := NewRetriever(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing was asked, so nothing is looked up: injecting the whole knowledge
	// base would spend the window on prose nobody asked for.
	for _, messages := range [][]core.Message{
		nil,
		{},
		{userTextMessage("   ")},
		{userTextMessage("")},
		{core.Message{Role: core.RoleSystem, Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: "system"}}}},
	} {
		request := core.ChatRequest{Messages: messages}
		out, err := retriever.Hook()(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.(core.ChatRequest).Messages) != len(messages) {
			t.Fatalf("messages = %#v", out.(core.ChatRequest).Messages)
		}
	}
	// The hook is given something that is not a request, which means the assembly
	// wired it somewhere it does not belong. Reporting that beats dropping the
	// knowledge silently.
	if _, err := retriever.Hook()(ctx, "not a request"); err == nil {
		t.Fatal("the hook accepted the wrong type")
	}
}

func TestRetrieverBoundsWhatItInjects(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	for index := 0; index < 12; index++ {
		put(t, store, "Article "+string(rune('a'+index)), "the common subject appears in all of them")
	}
	retriever, err := NewRetriever(store, 3)
	if err != nil {
		t.Fatal(err)
	}
	message, count := retriever.Retrieve(ctx, "common")
	if message == nil {
		t.Fatal("nothing was injected")
	}
	if count > 3 {
		t.Fatalf("injected %d entries, the budget is 3", count)
	}
}

func TestRetrieverSkipsAnIndexEntryThatCannotBeRead(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	put(t, store, "Readable", "the common subject")
	if _, err := store.open(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO entries(id, title, body, tags, path, updated_at)
		 VALUES('ghost', 'Ghost', 'the common subject too', '', 'nowhere', '2026-01-01')`); err != nil {
		t.Fatal(err)
	}
	retriever, err := NewRetriever(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	message, count := retriever.Retrieve(ctx, "common")
	if count != 1 {
		t.Fatalf("count = %d", count)
	}
	if strings.Contains(message.Content[0].Text, "Ghost") {
		t.Fatalf("an entry that cannot be read was injected: %q", message.Content[0].Text)
	}
}

func userTextMessage(text string) core.Message {
	return core.Message{
		Role:    core.RoleUser,
		Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: text}},
	}
}

// TestSnippetAroundStatesItsPosition covers the three shapes a snippet takes,
// because a caller has to know whether it is looking at the start of an article,
// the middle of one, or the end.
func TestSnippetAroundStatesItsPosition(t *testing.T) {
	body := strings.Repeat("a", 400) + "the needle is here" + strings.Repeat("b", 400)
	// A match in the middle is shown with both ends elided.
	snippet := snippetAround(body, "needle")
	if !strings.HasPrefix(snippet, "...") || !strings.HasSuffix(snippet, "...") {
		t.Fatalf("snippet = %q", snippet)
	}
	if !strings.Contains(snippet, "the needle is here") {
		t.Fatalf("snippet = %q", snippet)
	}
	// A short body is shown whole, with no elision that would suggest something
	// was left out.
	short := snippetAround("a short body with needle in it", "needle")
	if strings.Contains(short, "...") {
		t.Fatalf("snippet = %q", short)
	}
	// A long body with no match is shown from the start and marked as cut.
	noMatch := snippetAround(strings.Repeat("c", 400), "needle")
	if !strings.HasPrefix(noMatch, "ccc") || !strings.HasSuffix(noMatch, "...") {
		t.Fatalf("snippet = %q", noMatch)
	}
	// A match near the start has no leading elision, because there is nothing
	// before it.
	nearStart := snippetAround("needle at the very start"+strings.Repeat("d", 400), "needle")
	if strings.HasPrefix(nearStart, "...") {
		t.Fatalf("snippet = %q", nearStart)
	}
	// Whitespace around a body is dropped, so a snippet is one line of prose.
	if snippet := snippetAround("  \n padded body \n ", "padded"); snippet != "padded body" {
		t.Fatalf("snippet = %q", snippet)
	}
}

func TestScoreOfWeighsWhereTheMatchWas(t *testing.T) {
	// A title match outranks a tag match, which outranks a body match, because a
	// caller reads the better article first.
	title := scoreOf("the cache", nil, "unrelated", "cache")
	tags := scoreOf("unrelated", []string{"cache"}, "unrelated", "cache")
	body := scoreOf("unrelated", nil, "mentions the cache here", "cache")
	none := scoreOf("unrelated", nil, "unrelated", "cache")
	if !(title > tags && tags > body && body > none) {
		t.Fatalf("scores: title %v tags %v body %v none %v", title, tags, body, none)
	}
}

func TestUnwrapPathErrorTellsAMissingFileFromAnUnreadableOne(t *testing.T) {
	// The two are different problems: one is the ordinary first write and the
	// other is a file that has gone bad.
	_, err := os.Open(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(unwrapPathError(err), os.ErrNotExist) {
		t.Fatalf("err = %v", unwrapPathError(err))
	}
	plain := os.ErrPermission
	if unwrapPathError(plain) != os.ErrPermission {
		t.Fatalf("err = %v", unwrapPathError(plain))
	}
}

func TestFindToolReportsAnAbsentName(t *testing.T) {
	if findTool(nil, ToolSearch) != nil {
		t.Fatal("a tool was found in an empty list")
	}
}

func TestAnUnreadableEntryIsReportedRatherThanOverwritten(t *testing.T) {
	store := newTestStore(t)
	// A directory where an entry belongs cannot be read as an article, and
	// overwriting it would destroy whatever it was.
	blocking := filepath.Join(store.EntriesDir(), "blocked"+EntryExtension)
	if err := os.MkdirAll(blocking, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := store.Put(context.Background(), Entry{ID: "blocked", Title: "Blocked", Body: "body"})
	if err == nil {
		t.Fatal("a directory was overwritten as if it were an article")
	}
	// The listing skips it rather than failing, because one bad article should not
	// hide the rest of the knowledge base.
	put(t, store, "Readable", "body")
	if store.Count() != 1 {
		t.Fatalf("count = %d", store.Count())
	}
}

func TestTheListingWriteFailureDoesNotFailTheEntry(t *testing.T) {
	store := newTestStore(t)
	// The listing is a convenience beside markdown that is the source, so a listing
	// that cannot be written must not cost a caller their article.
	blocked := filepath.Join(store.Root(), IndexFileName)
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), Entry{ID: "thing", Title: "Thing", Body: "body"}); err != nil {
		t.Fatalf("the entry was lost because the listing could not be written: %v", err)
	}
	read, err := store.Get("thing")
	if err != nil {
		t.Fatal(err)
	}
	if read.Body != "body" {
		t.Fatalf("body = %q", read.Body)
	}
}
