// SPDX-License-Identifier: Apache-2.0

// Package wiki holds the project knowledge base: entries written as markdown a
// person can read and edit, an index a machine can search, and the two tools that
// connect them.
//
// The design makes the entries the source and the index a convenience, because a
// knowledge base nobody can read is a knowledge base nobody maintains. A search
// index that disagrees with the markdown on disk is a bug the rebuild fixes.
package wiki

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
	_ "modernc.org/sqlite"
)

// Directory and file names inside a scope.
const (
	// EntriesDirName holds the markdown.
	EntriesDirName = "entries"
	// IndexFileName holds the index, which is derived and therefore rebuildable.
	IndexFileName = "index.json"
	// IndexFileNameSearch is the search index, a database beside the markdown.
	IndexFileNameSearch = "search.db"
	// EntryExtension is what an entry is called on disk.
	EntryExtension = ".md"
)

// SchemaVersion is bumped when the index layout changes, so an index written by
// another version is rebuilt rather than queried with the wrong shape.
const SchemaVersion = 1

// Entry is one wiki article.
type Entry struct {
	// ID is the file name without its extension, and is the identifier every other
	// field refers to. A stable identifier matters because a search result is
	// something a caller caches and re-reads.
	ID string `json:"id"`
	// Title is the entry's heading, which is also its human readable name.
	Title string `json:"title"`
	// Tags are the words a search is likely to use, kept beside the body rather
	// than inside it so a query does not have to read every article.
	Tags []string `json:"tags,omitempty"`
	// Body is the markdown, without the heading.
	Body string `json:"body"`
	// Path is where the entry lives.
	Path string `json:"path"`
	// UpdatedAt is the file's own timestamp, which is also when it was created
	// because the markdown carries no separate creation time. An article rewritten
	// by hand has no creation time to preserve that anybody could read, so the
	// filesystem's answer is the honest one.
	UpdatedAt time.Time `json:"updated_at"`
	// Truncated records that the body was cut when it was written, because an entry
	// that silently lost its end is worse than a short one.
	Truncated bool `json:"truncated,omitempty"`
}

// MaxBodyBytes is the largest body an entry holds. A knowledge base entry is a
// summary of something larger, and a file that grows without limit is a file
// nobody will read or search.
const MaxBodyBytes = 64 << 10

// maxTitleBytes and maxTagBytes keep a single field from dominating an entry.
const (
	maxTitleBytes = 200
	maxTagBytes   = 64
	maxTags       = 32
)

// NewEntry builds an entry, trimming what cannot be stored.
//
// The identifier is derived from the title so a caller that only knows what a
// thing is called still gets a usable entry, and the title is what a human reads
// in a search result.
func NewEntry(title, body string, tags []string) (Entry, error) {
	trimmedTitle := strings.TrimSpace(title)
	if trimmedTitle == "" {
		return Entry{}, fmt.Errorf("wiki: an entry needs a title")
	}
	identifier, err := Slugify(trimmedTitle)
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{
		ID:        identifier,
		Title:     truncateBytes(trimmedTitle, maxTitleBytes),
		Tags:      normalizeTags(tags),
		Body:      body,
		UpdatedAt: time.Now().UTC(),
	}
	if len(entry.Body) > MaxBodyBytes {
		entry.Body = string([]byte(entry.Body)[:MaxBodyBytes])
		entry.Truncated = true
	}
	return entry, nil
}

// Slugify turns a title into an identifier that is safe as a file name on all
// three platforms.
//
// A separator that one platform treats specially is not a separator here: the
// identifier has to survive being a file name on Windows, which forbids the
// characters that are perfectly legal in a POSIX name.
func Slugify(title string) (string, error) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return "", fmt.Errorf("wiki: a title is needed to build an identifier")
	}
	var builder strings.Builder
	lastWasSeparator := false
	for _, character := range trimmed {
		switch {
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9':
			builder.WriteRune(character)
			lastWasSeparator = false
		case character >= 'A' && character <= 'Z':
			builder.WriteRune(character - 'A' + 'a')
			lastWasSeparator = false
		case character == ' ' || character == '-' || character == '_':
			if !lastWasSeparator && builder.Len() > 0 {
				builder.WriteByte('-')
				lastWasSeparator = true
			}
		}
		// Everything else, including a path separator and a character outside
		// ascii, is dropped rather than transliterated: a transliteration nobody
		// asked for is harder to predict than a missing letter.
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		// A title written entirely in a script with no ascii leaves nothing to build a
		// name from. A digest of the title is used instead of refusing, because an
		// article nobody can name is an article nobody can find, and a digest is both
		// stable across runs and safe as a file name everywhere.
		sum := sha256.Sum256([]byte(trimmed))
		return "entry-" + hex.EncodeToString(sum[:])[:16], nil
	}
	if len(slug) > 120 {
		slug = strings.Trim(slug[:120], "-")
	}
	return slug, nil
}

func normalizeTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed := strings.ToLower(strings.TrimSpace(tag))
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, truncateBytes(trimmed, maxTagBytes))
		if len(out) == maxTags {
			break
		}
	}
	// A stable order means a diff of two index files is readable.
	sort.Strings(out)
	return out
}

func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	// The cut is on a rune boundary, so a truncated title is still valid text.
	cut := limit
	for cut > 0 && value[cut]&0xC0 == 0x80 {
		cut--
	}
	return value[:cut]
}

// markdown renders an entry as the file on disk.
//
// The heading comes first because that is what a person opening the file expects
// to see, and the tags are a comment because they are metadata rather than prose.
func (e Entry) markdown() string {
	var builder strings.Builder
	builder.WriteString("# ")
	builder.WriteString(e.Title)
	builder.WriteString("\n\n")
	if len(e.Tags) > 0 {
		builder.WriteString("<!-- tags: ")
		builder.WriteString(strings.Join(e.Tags, ", "))
		builder.WriteString(" -->\n\n")
	}
	body := strings.TrimSpace(e.Body)
	if body != "" {
		builder.WriteString(body)
		builder.WriteString("\n")
	}
	return builder.String()
}

// parseMarkdown reads an entry back out of its file.
//
// A file that has been edited by hand is the normal case rather than an error, so
// anything that can be read is read and only a file with no usable heading is
// refused.
func parseMarkdown(path string) (Entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, fmt.Errorf("wiki: read the entry: %w", err)
	}
	text := string(raw)
	entry := Entry{
		Path:      path,
		ID:        strings.TrimSuffix(filepath.Base(path), EntryExtension),
		Tags:      []string{},
		Truncated: false,
		UpdatedAt: fileModTime(path),
	}
	var body strings.Builder
	// The metadata comment is recognised anywhere in the leading block rather than
	// only on the first line, because a file someone edited by hand may well have a
	// blank line between the heading and it.
	inLeadingBlock := true
	for index, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if index == 0 && strings.HasPrefix(trimmed, "# ") {
			entry.Title = strings.TrimSpace(trimmed[2:])
			continue
		}
		if inLeadingBlock {
			if strings.HasPrefix(trimmed, "<!-- tags:") {
				if tags, _, found := strings.Cut(trimmed, "-->"); found {
					inner := strings.TrimSuffix(strings.TrimPrefix(tags, "<!-- tags:"), " ")
					entry.Tags = normalizeTags(strings.Split(inner, ","))
					continue
				}
			}
			if trimmed == "" {
				// The blank lines that pad the header are layout, not prose, and
				// keeping them would make every round trip grow the body.
				continue
			}
			// The first line of prose ends the block where metadata may appear.
			inLeadingBlock = false
		}
		body.WriteString(trimmed)
		body.WriteString("\n")
	}
	if entry.Title == "" {
		// A file with no heading is titled after itself, so an entry that someone
		// created by hand is still findable rather than refused.
		entry.Title = strings.TrimSpace(strings.ReplaceAll(entry.ID, "-", " "))
	}
	if entry.Title == "" {
		return Entry{}, fmt.Errorf("wiki: %q has no title and no identifier", path)
	}
	entry.Body = strings.TrimSpace(body.String())
	entry.Truncated = len(entry.Body) > MaxBodyBytes
	return entry, nil
}

func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

// searchSchema is the search index.
//
// The markdown stays the source of truth, so this table holds only what a search
// needs: the text to match and where the entry it came from lives.
const searchSchema = `
CREATE TABLE IF NOT EXISTS wiki_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS entries (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL,
    tags        TEXT NOT NULL,
    path        TEXT NOT NULL,
    updated_at  DATETIME NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS entries_fts USING fts5(
    title, body, tags, content='entries', content_rowid='rowid', tokenize='trigram'
);
CREATE TRIGGER IF NOT EXISTS entries_ai AFTER INSERT ON entries BEGIN
    INSERT INTO entries_fts(rowid, title, body, tags)
    VALUES (new.rowid, new.title, new.body, new.tags);
END;
CREATE TRIGGER IF NOT EXISTS entries_ad AFTER DELETE ON entries BEGIN
    INSERT INTO entries_fts(entries_fts, rowid, title, body, tags)
    VALUES ('delete', old.rowid, old.title, old.body, old.tags);
END;
CREATE TRIGGER IF NOT EXISTS entries_au AFTER UPDATE ON entries BEGIN
    INSERT INTO entries_fts(entries_fts, rowid, title, body, tags)
    VALUES ('delete', old.rowid, old.title, old.body, old.tags);
    INSERT INTO entries_fts(rowid, title, body, tags)
    VALUES (new.rowid, new.title, new.body, new.tags);
END;
`

// Store holds the entries and the index beside them.
type Store struct {
	// root is the wiki directory, which the storage scope owns.
	root string
	// entriesDir is where the markdown lives.
	entriesDir string
	// searchPath is the index database.
	searchPath string
	// db is the index, opened lazily so a wiki that is only written is still usable.
	db *sql.DB
	// clock is the time source, so a test can make entries deterministic.
	clock func() time.Time
}

// NewStore opens a wiki in a directory, creating the layout.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("wiki: a store needs a directory")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("wiki: resolve the wiki directory: %w", err)
	}
	entriesDir := filepath.Join(absolute, EntriesDirName)
	if err := os.MkdirAll(entriesDir, 0o755); err != nil {
		return nil, fmt.Errorf("wiki: create the entries directory: %w", err)
	}
	store := &Store{
		root:       absolute,
		entriesDir: entriesDir,
		searchPath: filepath.Join(absolute, IndexFileNameSearch),
		clock:      func() time.Time { return time.Now().UTC() },
	}
	return store, nil
}

// Root reports the wiki directory.
func (s *Store) Root() string { return s.root }

// EntriesDir reports where the markdown lives.
func (s *Store) EntriesDir() string { return s.entriesDir }

// IndexPath reports the search index.
func (s *Store) IndexPath() string { return s.searchPath }

func (s *Store) open() (*sql.DB, error) {
	if s.db != nil {
		return s.db, nil
	}
	dsn := "file:" + filepath.ToSlash(s.searchPath) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("wiki: open the search index: %w", err)
	}
	// One writer keeps the index consistent, and a wiki is small enough that a
	// second connection would only add contention.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(searchSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("wiki: create the search index: %w", err)
	}
	version := fmt.Sprint(SchemaVersion)
	if err := db.QueryRow(`SELECT value FROM wiki_meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			_ = db.Close()
			return nil, fmt.Errorf("wiki: read the index version: %w", err)
		}
		version = ""
	}
	if version != fmt.Sprint(SchemaVersion) {
		// An index written by another version is dropped rather than queried with a
		// shape this code does not understand. The markdown is untouched, so a
		// rebuild recovers it.
		for _, table := range []string{"entries_fts", "entries", "wiki_meta"} {
			if _, err := db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("wiki: drop the stale index: %w", err)
			}
		}
		if _, err := db.Exec(searchSchema); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("wiki: recreate the search index: %w", err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO wiki_meta(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		fmt.Sprint(SchemaVersion)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("wiki: record the index version: %w", err)
	}
	s.db = db
	return db, nil
}

// Close releases the index.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		_ = s.db.Close()
		s.db = nil
		return fmt.Errorf("wiki: checkpoint the search index: %w", err)
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// entryPath reports where an entry's file lives.
func (s *Store) entryPath(id string) (string, error) {
	if err := ValidateID("entry id", id); err != nil {
		return "", err
	}
	return filepath.Join(s.entriesDir, id+EntryExtension), nil
}

// ValidateID rejects an identifier that would escape the entries directory. An
// identifier reaches this code from a search result and from a tool call, so it is
// never trusted as a path component.
func ValidateID(label, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("wiki: %s must not be empty", label)
	}
	if id == "." || id == ".." || strings.ContainsAny(id, `/\\:`) {
		return fmt.Errorf("wiki: %s %q is not a valid identifier", label, id)
	}
	if strings.ContainsRune(id, 0) {
		return fmt.Errorf("wiki: %s %q contains a null byte", label, id)
	}
	return nil
}

// Put writes an entry and puts it in the index.
//
// The markdown is written first, because it is the source: an index that is ahead
// of the files would report an entry that cannot be read back.
func (s *Store) Put(ctx context.Context, entry Entry) (Entry, error) {
	if err := ValidateID("entry id", entry.ID); err != nil {
		return Entry{}, err
	}
	path, err := s.entryPath(entry.ID)
	if err != nil {
		return Entry{}, err
	}
	existing, readErr := parseMarkdown(path)
	if readErr != nil {
		// A file that is present but unreadable is a problem worth reporting, while
		// one that is simply absent is the ordinary first write.
		if _, statErr := os.Stat(path); statErr == nil {
			return Entry{}, fmt.Errorf("wiki: the entry is unreadable: %w", readErr)
		}
	}
	if entry.Title == "" {
		// A rewrite that names no title keeps the one the file already has, so a
		// partial update cannot silently rename an article.
		entry.Title = existing.Title
	}
	if entry.Title == "" {
		return Entry{}, fmt.Errorf("wiki: an entry needs a title")
	}
	entry.Path = path
	entry.UpdatedAt = s.clock()
	entry.Tags = normalizeTags(entry.Tags)
	if len(entry.Body) > MaxBodyBytes {
		entry.Body = string([]byte(entry.Body)[:MaxBodyBytes])
		entry.Truncated = true
	}
	// The file goes to a temporary name and is renamed into place, so a reader
	// never sees half an article and a crash never leaves one.
	temporary := path + ".partial"
	if err := os.WriteFile(temporary, []byte(entry.markdown()), 0o600); err != nil {
		return Entry{}, fmt.Errorf("wiki: write the entry: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return Entry{}, fmt.Errorf("wiki: publish the entry: %w", err)
	}
	if err := s.index(ctx, entry); err != nil {
		return Entry{}, err
	}
	s.writeIndexFile()
	return entry, nil
}

// index puts one entry into the search index.
func (s *Store) index(ctx context.Context, entry Entry) error {
	db, err := s.open()
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO entries(id, title, body, tags, path, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title = excluded.title, body = excluded.body, tags = excluded.tags,
		   path = excluded.path, updated_at = excluded.updated_at`,
		entry.ID, entry.Title, entry.Body, strings.Join(entry.Tags, " "),
		entry.Path, entry.UpdatedAt); err != nil {
		return fmt.Errorf("wiki: index the entry: %w", err)
	}
	return nil
}

// indexFile is the human readable listing beside the markdown. It exists so a
// person can see what is there without a search tool.
type indexFile struct {
	Version   int       `json:"version"`
	Generated time.Time `json:"generated"`
	Entries   []Entry   `json:"entries"`
}

// writeIndexFile rewrites the listing.
func (s *Store) writeIndexFile() {
	entries := s.List()
	encoded, err := json.MarshalIndent(indexFile{
		Version:   SchemaVersion,
		Generated: s.clock(),
		Entries:   entries,
	}, "", "  ")
	if err != nil {
		// The listing is a convenience and the markdown is the source, so a
		// listing that will not encode is not worth failing a write over.
		return
	}
	path := filepath.Join(s.root, IndexFileName)
	temporary := path + ".partial"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
	}
}

// Get reads one entry back.
func (s *Store) Get(id string) (Entry, error) {
	path, err := s.entryPath(id)
	if err != nil {
		return Entry{}, err
	}
	entry, err := parseMarkdown(path)
	if err != nil {
		if os.IsNotExist(unwrapPathError(err)) || errors.Is(err, os.ErrNotExist) {
			return Entry{}, fmt.Errorf("%w: %q", ErrNoSuchEntry, id)
		}
		return Entry{}, err
	}
	return entry, nil
}

// ErrNoSuchEntry is returned when an entry does not exist.
var ErrNoSuchEntry = errors.New("wiki: there is no such entry")

// Delete removes an entry from the files and the index.
func (s *Store) Delete(ctx context.Context, id string) error {
	path, err := s.entryPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %q", ErrNoSuchEntry, id)
		}
		return fmt.Errorf("wiki: remove the entry: %w", err)
	}
	if db, err := s.open(); err == nil {
		if _, err := db.ExecContext(ctx, `DELETE FROM entries WHERE id = ?`, id); err != nil {
			return fmt.Errorf("wiki: remove the entry from the index: %w", err)
		}
	}
	s.writeIndexFile()
	return nil
}

// List returns every entry, ordered by identifier so a listing is stable.
func (s *Store) List() []Entry {
	entries, err := os.ReadDir(s.entriesDir)
	if err != nil {
		return nil
	}
	out := make([]Entry, 0, len(entries))
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), EntryExtension) {
			continue
		}
		entry, err := parseMarkdown(filepath.Join(s.entriesDir, item.Name()))
		if err != nil {
			// A file that will not parse is skipped rather than failing the listing:
			// one broken article should not hide the rest of the knowledge base.
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(first, second int) bool { return out[first].ID < out[second].ID })
	return out
}

// Count reports how many entries there are.
func (s *Store) Count() int { return len(s.List()) }

// Rebuild reindexes every file on disk.
//
// The markdown is the source, so this is how an index that drifted, or one written
// by another version, is brought back without anyone having to remember what was
// in it.
func (s *Store) Rebuild(ctx context.Context) (int, error) {
	entries := s.List()
	db, err := s.open()
	if err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM entries`); err != nil {
		return 0, fmt.Errorf("wiki: clear the index: %w", err)
	}
	indexed := 0
	for _, entry := range entries {
		if err := s.index(ctx, entry); err != nil {
			return indexed, err
		}
		indexed++
	}
	s.writeIndexFile()
	return indexed, nil
}

// Hit is one search result.
type Hit struct {
	// EntryID identifies the entry, which is what a caller reads back.
	EntryID string `json:"entry_id"`
	// Title and Tags come back with the hit so a result is readable without a
	// second call.
	Title string   `json:"title"`
	Tags  []string `json:"tags,omitempty"`
	// Snippet is the part of the body that matched, which is what a caller decides
	// on.
	Snippet string `json:"snippet"`
	// Score orders the results. A higher score is a better match.
	Score float64 `json:"score"`
}

// Search finds entries by substring in the title, the body or the tags.
//
// A query of at least three characters uses the trigram index, which is what makes
// a substring query work in a dense script as well as in english. A shorter query
// falls back to a scan, because a tokenizer cannot index a fragment that short.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, fmt.Errorf("wiki: a search needs something to look for")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	entries := s.List()
	byID := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
	}

	if len([]rune(trimmed)) < 3 {
		return s.scanFor(ctx, byID, trimmed, limit), nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT e.id, e.title, e.tags, e.body
		 FROM entries_fts f JOIN entries e ON e.rowid = f.rowid
		 WHERE entries_fts MATCH ? ORDER BY rank LIMIT ?`,
		ftsQuery(trimmed), limit*4)
	if err != nil {
		return nil, fmt.Errorf("wiki: search: %w", err)
	}
	defer rows.Close()

	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var id, title, tags, body string
		if err := rows.Scan(&id, &title, &tags, &body); err != nil {
			return nil, fmt.Errorf("wiki: read a search result: %w", err)
		}
		entry, known := byID[id]
		if !known {
			// The index holds something the files no longer do, which means it has
			// drifted. Skipping it is better than reporting an entry that cannot be
			// read back.
			continue
		}
		hits = append(hits, Hit{
			EntryID: entry.ID,
			Title:   entry.Title,
			Tags:    entry.Tags,
			Snippet: snippetAround(body, trimmed),
			Score:   scoreOf(title, entry.Tags, body, trimmed),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("wiki: read the search results: %w", err)
	}
	sort.SliceStable(hits, func(first, second int) bool {
		if hits[first].Score != hits[second].Score {
			return hits[first].Score > hits[second].Score
		}
		return hits[first].EntryID < hits[second].EntryID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// scanFor is the short query path.
func (s *Store) scanFor(_ context.Context, byID map[string]Entry, query string, limit int) []Hit {
	lowered := strings.ToLower(query)
	hits := make([]Hit, 0, limit)
	for _, entry := range byID {
		position := indexFold(entry.Title, lowered)
		weight := 3.0
		if position < 0 {
			if position = indexFold(strings.Join(entry.Tags, " "), lowered); position < 0 {
				position = indexFold(entry.Body, lowered)
				weight = 1.0
			} else {
				weight = 2.0
			}
		}
		if position < 0 {
			continue
		}
		hits = append(hits, Hit{
			EntryID: entry.ID,
			Title:   entry.Title,
			Tags:    entry.Tags,
			Snippet: snippetAround(entry.Body, query),
			Score:   weight,
		})
		if len(hits) == limit {
			break
		}
	}
	sort.SliceStable(hits, func(first, second int) bool {
		if hits[first].Score != hits[second].Score {
			return hits[first].Score > hits[second].Score
		}
		return hits[first].EntryID < hits[second].EntryID
	})
	return hits
}

// indexFold finds a substring without regard to case, reporting where it starts.
func indexFold(haystack, needleLower string) int {
	lowered := strings.ToLower(haystack)
	return strings.Index(lowered, needleLower)
}

// snippetAround returns the part of a body around a match, with its position
// stated, because a caller needs to know where in the article the match was.
func snippetAround(body, query string) string {
	trimmed := strings.TrimSpace(body)
	position := indexFold(trimmed, strings.ToLower(query))
	if position < 0 {
		if len(trimmed) <= 160 {
			return trimmed
		}
		return truncateBytes(trimmed, 157) + "..."
	}
	start := position - 60
	if start < 0 {
		start = 0
	}
	end := position + 100
	if end > len(trimmed) {
		end = len(trimmed)
	}
	snippet := trimmed[start:end]
	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(trimmed) {
		snippet += "..."
	}
	return snippet
}

// scoreOf weights a hit by where the match was, so a title match outranks a body
// match and a caller reads the better article first.
func scoreOf(title string, tags []string, body, query string) float64 {
	lowered := strings.ToLower(query)
	score := 0.0
	if indexFold(title, lowered) >= 0 {
		score += 3
	}
	if indexFold(strings.Join(tags, " "), lowered) >= 0 {
		score += 2
	}
	if indexFold(body, lowered) >= 0 {
		score++
	}
	return score
}

// ftsQuery quotes the caller's text so a query made of punctuation cannot be read
// as query syntax.
func ftsQuery(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

// unwrapPathError returns the error behind a path failure, which is what a caller
// needs to tell a missing file from an unreadable one.
func unwrapPathError(err error) error {
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return pathError.Err
	}
	return err
}

// Tools returns the two tools the wiki exposes.
func (s *Store) Tools() []core.Tool {
	return []core.Tool{&searchTool{store: s}, &readTool{store: s}}
}

// Tool names, which are the only names the core loop sees.
const (
	// ToolSearch finds entries.
	ToolSearch = "wiki_search"
	// ToolRead reads one entry.
	ToolRead = "wiki_read"
)

// findTool returns a tool by name, for a caller that was handed the list.
func findTool(tools []core.Tool, name string) core.Tool {
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	return nil
}
