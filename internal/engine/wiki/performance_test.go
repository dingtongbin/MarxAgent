// SPDX-License-Identifier: Apache-2.0

package wiki

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// A search runs on every turn that asks a question, and an entry is written
// whenever a sub agent learns something, so neither may grow worse than linearly
// with the knowledge base.

func TestWikiPerformanceGates(t *testing.T) {
	if os.Getenv("MARXAGENT_PERFORMANCE") != "1" {
		t.Skip("set MARXAGENT_PERFORMANCE=1 to run the wiki performance gates")
	}
	t.Run("a_search_does_not_read_every_article", func(t *testing.T) {
		ctx := context.Background()
		store := newTestStore(t)
		// A base large enough that a full scan would show up, with one entry that
		// matches and many that do not.
		put(t, store, "The one that matches", "the distinctive phrase appears here", "findme")
		for index := 0; index < 300; index++ {
			put(t, store, "Filler "+string(rune('a'+index%26))+" article",
				strings.Repeat("ordinary prose about ordinary things. ", 20))
		}
		started := time.Now()
		hits, err := store.Search(ctx, "distinctive", 10)
		if err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(started)
		if len(hits) != 1 {
			t.Fatalf("hits = %#v", hits)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("a search over %d entries took %v", store.Count(), elapsed)
		}
		t.Logf("a search over %d entries took %v", store.Count(), elapsed)
	})
	t.Run("writing_one_entry_does_not_rewrite_the_base", func(t *testing.T) {
		store := newTestStore(t)
		for index := 0; index < 100; index++ {
			put(t, store, "Existing "+string(rune('a'+index%26))+" article", "existing body")
		}
		// The listing beside the markdown is a convenience, so a write that indexes
		// one entry must not cost a pass over every file. The gate is a bound rather
		// than a measurement, because the pass is what would be quadratic.
		started := time.Now()
		put(t, store, "One more article", "one more body")
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("writing one entry took %v", elapsed)
		}
	})
	t.Run("a_short_query_still_terminates", func(t *testing.T) {
		ctx := context.Background()
		store := newTestStore(t)
		for index := 0; index < 200; index++ {
			put(t, store, "Article "+string(rune('a'+index%26)), "body with the letter z in it")
		}
		// A query too short to index falls back to a scan, and that scan is the
		// slowest path, so it is the one that has to stay bounded.
		started := time.Now()
		hits, err := store.Search(ctx, "z", 20)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			t.Fatal("the scan found nothing")
		}
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("a scanned query over %d entries took %v", store.Count(), elapsed)
		}
	})
	t.Run("retrieval_is_bounded_by_its_budget", func(t *testing.T) {
		ctx := context.Background()
		store := newTestStore(t)
		for index := 0; index < 60; index++ {
			put(t, store, "Article "+string(rune('a'+index%26))+string(rune('a'+index/26)),
				"the shared subject appears in all of them")
		}
		retriever, err := NewRetriever(store, 4)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		message, count := retriever.Retrieve(ctx, "shared")
		if elapsed := time.Since(started); elapsed > 3*time.Second {
			t.Fatalf("retrieval took %v", elapsed)
		}
		if message == nil || count > 4 {
			t.Fatalf("count = %d", count)
		}
	})
}

func BenchmarkSearchIndexed(b *testing.B) {
	ctx := context.Background()
	store, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	for index := 0; index < 200; index++ {
		entry := Entry{
			ID:    "article-" + string(rune('a'+index%26)) + string(rune('a'+index/26)),
			Title: "An article", Body: strings.Repeat("ordinary prose. ", 40),
		}
		if _, err := store.Put(ctx, entry); err != nil {
			b.Fatal(err)
		}
	}
	put, err := NewEntry("Findable", "the distinctive phrase", nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := store.Put(ctx, put); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := store.Search(ctx, "distinctive", 10); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPutEntry(b *testing.B) {
	ctx := context.Background()
	store, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	body := strings.Repeat("a body of prose. ", 50)
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		entry := Entry{
			ID:    "entry-" + string(rune('a'+iteration%26)) + string(rune('a'+iteration/26)),
			Title: "An entry", Body: body, Tags: []string{"one", "two"},
		}
		if _, err := store.Put(ctx, entry); err != nil {
			b.Fatal(err)
		}
	}
}
