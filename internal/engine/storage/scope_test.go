// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenScopeRejectsUnknownKindsAndRoots(t *testing.T) {
	if _, err := OpenScope("nonsense", t.TempDir()); err == nil {
		t.Fatal("an unknown scope kind was accepted")
	}
	if _, err := OpenScope(ScopeProject, "  "); err == nil {
		t.Fatal("an empty root was accepted")
	}
}

func TestScopeLayoutFollowsTheDesign(t *testing.T) {
	root := t.TempDir()
	scope, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()

	if scope.Kind() != ScopeProject {
		t.Fatalf("kind = %q", scope.Kind())
	}
	// A project scope keeps its data inside the repository so it is auditable
	// next to the code it describes.
	if scope.DataRoot() != filepath.Join(root, ProjectDirName) {
		t.Fatalf("data root = %q", scope.DataRoot())
	}
	for _, relative := range []string{
		filepath.Join(ProjectDirName, DBFileName),
		filepath.Join(ProjectDirName, SessionsDirName),
		filepath.Join(ProjectDirName, BrokerDirName),
		filepath.Join(ProjectDirName, WikiDirName),
	} {
		if _, err := os.Stat(filepath.Join(root, relative)); err != nil {
			t.Fatalf("%s was not created: %v", relative, err)
		}
	}
	sessionPath, eventsPath, err := scope.SessionJournals("s1")
	if err != nil {
		t.Fatal(err)
	}
	if sessionPath != filepath.Join(root, ProjectDirName, "sessions", "s1", "session.jsonl") {
		t.Fatalf("session journal = %q", sessionPath)
	}
	if eventsPath != filepath.Join(root, ProjectDirName, "sessions", "s1", "events.jsonl") {
		t.Fatalf("events journal = %q", eventsPath)
	}
	if scope.BrokerJournalPath() != filepath.Join(root, ProjectDirName, "broker", "journal.jsonl") {
		t.Fatalf("broker journal = %q", scope.BrokerJournalPath())
	}
	if scope.LSPCachePath() != filepath.Join(root, ProjectDirName, "cache.json") {
		t.Fatalf("lsp cache = %q", scope.LSPCachePath())
	}
}

func TestAppDataScopeHasNoWikiDirectory(t *testing.T) {
	scope, err := OpenScope(ScopeAppData, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	// Chat mode has no knowledge base and no language server, so it must not
	// create directories the work mode owns.
	if _, err := os.Stat(scope.WikiDir()); !os.IsNotExist(err) {
		t.Fatalf("the wiki directory exists: %v", err)
	}
	if _, err := os.Stat(scope.SessionsDir()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenSessionJournalsCreatesTheLayout(t *testing.T) {
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	sessionJournal, eventsJournal, err := scope.OpenSessionJournals("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionJournal.Write(context.Background(), []Record{messageRecord(1, "m1", "user", "hi")}); err != nil {
		t.Fatal(err)
	}
	if err := eventsJournal.Write(context.Background(), []Record{messageRecord(1, "e1", "user", "hi")}); err != nil {
		t.Fatal(err)
	}
	if err := sessionJournal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := eventsJournal.Close(); err != nil {
		t.Fatal(err)
	}
	// Both journals of a session have to be discoverable by recovery, which is
	// what makes the two track one truth.
	journals, err := discoverJournals(scope.DataRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(journals) != 2 {
		t.Fatalf("journals = %#v", journals)
	}
	streams := []string{journals[0].stream, journals[1].stream}
	if !containsString(streams, "session:s1") || !containsString(streams, "events:s1") {
		t.Fatalf("streams = %v", streams)
	}
}

func TestSessionStreamsAndBrokerJournal(t *testing.T) {
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	streams := scope.SessionStreams("s1")
	if len(streams) != 2 || streams[0] != "session:s1" || streams[1] != "events:s1" {
		t.Fatalf("streams = %v", streams)
	}
	broker, err := scope.OpenBrokerJournal()
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.Write(context.Background(), []Record{{
		Stream: "broker:journal", Seq: 1, Type: RecordBrokerMessage, Data: []byte(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	journals, err := discoverJournals(scope.DataRoot())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, journal := range journals {
		if journal.stream == "broker:journal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the broker journal was not discovered: %#v", journals)
	}
}

func TestSessionSummariesRoundTrip(t *testing.T) {
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	ctx := context.Background()
	// A session with no summary is normal early on, so absence is not an error.
	summary, err := scope.ReadSessionSummary("s1")
	if err != nil {
		t.Fatal(err)
	}
	if summary.SessionID != "" {
		t.Fatalf("summary = %#v", summary)
	}
	written := SessionSummary{
		SessionID:    "s1",
		Mode:         "work",
		CreatedAt:    time.Unix(100, 0).UTC(),
		UpdatedAt:    time.Unix(200, 0).UTC(),
		MessageCount: 7,
		LastSeq:      42,
		Summary:      "a work session",
	}
	if err := scope.WriteSessionSummary(written); err != nil {
		t.Fatal(err)
	}
	summary, err = scope.ReadSessionSummary("s1")
	if err != nil {
		t.Fatal(err)
	}
	if summary != written {
		t.Fatalf("summary = %#v, want %#v", summary, written)
	}
	if err := scope.Checkpoint(ctx, "s1", 42, 7, "a work session"); err != nil {
		t.Fatal(err)
	}
	seq, count, text, err := scope.Audit().Checkpoint(ctx, "session:s1")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 42 || count != 7 || text != "a work session" {
		t.Fatalf("checkpoint = %d %d %q", seq, count, text)
	}
}

func TestListSessionsFallsBackToTheDirectoryName(t *testing.T) {
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	if sessions, err := scope.ListSessions(); err != nil || len(sessions) != 0 {
		t.Fatalf("sessions = %#v err = %v", sessions, err)
	}
	if err := scope.WriteSessionSummary(SessionSummary{SessionID: "s2", MessageCount: 1}); err != nil {
		t.Fatal(err)
	}
	// A session directory with no summary is still listed, so a crash between
	// the first record and the first summary does not hide the session.
	if err := os.MkdirAll(filepath.Join(scope.SessionsDir(), "s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	sessions, err := scope.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %#v", sessions)
	}
	if sessions[0].SessionID != "s1" || sessions[1].SessionID != "s2" {
		t.Fatalf("sessions = %#v", sessions)
	}
}

func TestListSessionsReportsABrokenSummary(t *testing.T) {
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	directory, err := scope.EnsureSessionDir("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, SummaryFileName), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.ListSessions(); err == nil {
		t.Fatal("a broken summary was accepted")
	}
}

func TestValidateIDRejectsPathTraversal(t *testing.T) {
	for _, id := range []string{"", "   ", ".", "..", "a/b", `a\b`, "a:b", "a\x00b"} {
		if err := ValidateID("session id", id); err == nil {
			t.Fatalf("id %q was accepted", id)
		}
	}
	if err := ValidateID("session id", "s1-2_3"); err != nil {
		t.Fatal(err)
	}
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	if _, err := scope.SessionDir("../escape"); err == nil {
		t.Fatal("a traversal reached the session directory")
	}
}

func TestScopeRecoversItsOwnLayout(t *testing.T) {
	ctx := context.Background()
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	sessionJournal, eventsJournal, err := scope.OpenSessionJournals("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionJournal.Write(ctx, []Record{
		messageRecord(1, "m1", "user", "scoped needle"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := eventsJournal.Write(ctx, []Record{
		{Stream: "events:s1", Seq: 1, Type: RecordEvent, Data: []byte(`{"type":"state_change"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sessionJournal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := eventsJournal.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := scope.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("sessions = %#v", result.Sessions)
	}
	results, err := scope.Audit().SearchMessages(ctx, "s1", "scoped", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
}

func TestFileLockIsExclusivePerScope(t *testing.T) {
	root := t.TempDir()
	first, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Lock().Held() {
		t.Fatal("the scope did not take its own lock")
	}
	// A second handle in this process is a different file description, so it
	// must not be able to take the same lock.
	second, err := NewFileLock(filepath.Join(root, ProjectDirName, DBFileName+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Acquire(); err == nil {
		_ = second.Release()
		t.Fatal("a second lock was granted on the same scope")
	}
	if err := first.Lock().Acquire(); err != nil {
		t.Fatalf("reacquiring an held lock failed: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if first.Lock().Held() {
		t.Fatal("the lock was still held after the scope closed")
	}
	// Once released the scope can be reopened, which is what a restart needs.
	reopened, err := ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileLockReleaseIsIdempotent(t *testing.T) {
	lock, err := NewFileLock(filepath.Join(t.TempDir(), "scope.lock"))
	if err != nil {
		t.Fatal(err)
	}
	// Releasing a lock that was never taken is a no op so the shutdown path has
	// no ordering requirement.
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if lock.Held() {
		t.Fatal("an unheld lock reported itself held")
	}
	if err := lock.Acquire(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileLock("  "); err == nil {
		t.Fatal("an empty lock path was accepted")
	}
}

func TestAppDataScopeUsesTheUserConfigDirectory(t *testing.T) {
	// The real user configuration directory is redirected so the test never
	// writes into the developer's profile, on any of the three platforms.
	config := t.TempDir()
	t.Setenv("AppData", config)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("HOME", config)
	scope, err := AppDataScope()
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	if scope.Kind() != ScopeAppData {
		t.Fatalf("kind = %q", scope.Kind())
	}
	if !strings.HasSuffix(scope.Root(), filepath.Join(AppDirName)) {
		t.Fatalf("root = %q, want a path ending in %q", scope.Root(), AppDirName)
	}
	if !strings.HasPrefix(scope.Root(), config) {
		t.Fatalf("root = %q is not under the redirected config directory %q", scope.Root(), config)
	}
	if _, err := os.Stat(scope.SessionsDir()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenScopeReportsAnUnusableLockPath(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, ProjectDirName)
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory where the lock file belongs makes the lock untakeable, and a
	// scope that cannot be locked must be reported rather than opened anyway.
	if err := os.MkdirAll(filepath.Join(data, DBFileName+".lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ProjectScope(root); err == nil {
		t.Fatal("a scope with an unusable lock path was opened")
	}
}

func TestOpenSessionJournalsReportsAnUnusableJournal(t *testing.T) {
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	directory, err := scope.EnsureSessionDir("s1")
	if err != nil {
		t.Fatal(err)
	}
	// The second journal cannot be opened, and the first one has to be closed
	// again rather than leaking a handle.
	if err := os.MkdirAll(filepath.Join(directory, "events.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scope.OpenSessionJournals("s1"); err == nil {
		t.Fatal("an unusable journal path was accepted")
	}
}

func TestWriteSessionSummaryReportsAnUnusablePath(t *testing.T) {
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	directory, err := scope.EnsureSessionDir("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, SummaryFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := scope.WriteSessionSummary(SessionSummary{SessionID: "s1"}); err == nil {
		t.Fatal("an unusable summary path was accepted")
	}
}

func TestFileLockReportsAnUnusablePath(t *testing.T) {
	root := t.TempDir()
	// A file where a lock directory belongs makes the handle untakeable.
	blocker := filepath.Join(root, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileLock(filepath.Join(blocker, "scope.lock")); err == nil {
		t.Fatal("an unusable lock directory was accepted")
	}
	if _, err := LockFile(root); err == nil {
		t.Fatal("a directory was accepted as a lock file")
	}
}

func TestOpenScopeReportsAnUnusableLayout(t *testing.T) {
	// A file where a scope directory belongs must be reported, not ignored.
	fileAtSessions := t.TempDir()
	if err := os.WriteFile(filepath.Join(fileAtSessions, SessionsDirName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenScope(ScopeAppData, fileAtSessions); err == nil {
		t.Fatal("a file was accepted as the sessions directory")
	}
	// A directory where the ledger belongs has to fail the same way.
	fileAtDatabase := t.TempDir()
	data := filepath.Join(fileAtDatabase, "data")
	if err := os.MkdirAll(filepath.Join(data, DBFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenScope(ScopeAppData, data); err == nil {
		t.Fatal("a directory was accepted as the ledger")
	}
	// A file where the wiki directory belongs is a project layout failure.
	fileAtWiki := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fileAtWiki, SessionsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fileAtWiki, BrokerDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fileAtWiki, WikiDirName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenScope(ScopeProject, fileAtWiki); err == nil {
		t.Fatal("a file was accepted as the wiki directory")
	}
}

func TestUnlockFileIgnoresANilHandle(t *testing.T) {
	// The defensive branch matters because Release can be called on a lock that
	// was never taken.
	if err := unlockFile(nil); err != nil {
		t.Fatal(err)
	}
}

func TestScopeMarkerTracksTheLedgerHalf(t *testing.T) {
	ctx := context.Background()
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	marker, err := scope.Marker()
	if err != nil {
		t.Fatal(err)
	}
	// The ledger half alone is not enough: with no sentinel the marker is dirty.
	if err := scope.Audit().SetShutdownState(ctx, ShutdownStateClean); err != nil {
		t.Fatal(err)
	}
	clean, err := marker.IsClean(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Fatal("one half of the marker was accepted")
	}
	if err := marker.MarkClean(ctx); err != nil {
		t.Fatal(err)
	}
	if state, err := scope.Audit().ShutdownState(ctx); err != nil || state != ShutdownStateClean {
		t.Fatalf("ledger state = %q err = %v", state, err)
	}
	// Marking dirty has to move both halves, otherwise a restart would treat an
	// abandoned shutdown as orderly.
	if err := marker.MarkDirty(ctx); err != nil {
		t.Fatal(err)
	}
	if state, err := scope.Audit().ShutdownState(ctx); err != nil || state != ShutdownStateDirty {
		t.Fatalf("ledger state after dirty = %q err = %v", state, err)
	}
	if clean, err = marker.IsClean(ctx); err != nil || clean {
		t.Fatalf("clean = %v err = %v", clean, err)
	}
	if err := marker.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(scope.DataRoot(), CleanSentinelName)); !os.IsNotExist(err) {
		t.Fatalf("the sentinel survived removal: %v", err)
	}
}

func TestScopeMarkerReportsAnUnreadableRoot(t *testing.T) {
	// A scope root that is a file cannot hold a sentinel, which has to be
	// reported rather than treated as dirty.
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCleanMarker(file, nil); err == nil {
		t.Fatal("a file was accepted as a marker root")
	}
}

func TestScopeAccessorsReportTheirPaths(t *testing.T) {
	scope, err := ProjectScope(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	if !filepath.IsAbs(scope.Root()) {
		t.Fatalf("root = %q, want an absolute path", scope.Root())
	}
	if scope.LogsDir() != filepath.Join(scope.DataRoot(), "logs") {
		t.Fatalf("logs dir = %q", scope.LogsDir())
	}
	if scope.Lock().Path() != filepath.Join(scope.DataRoot(), DBFileName+".lock") {
		t.Fatalf("lock path = %q", scope.Lock().Path())
	}
	marker, err := scope.Marker()
	if err != nil {
		t.Fatal(err)
	}
	// The marker a scope hands out must agree with the scope's own ledger, or a
	// restart could read a marker that was never recorded.
	if err := marker.MarkClean(context.Background()); err != nil {
		t.Fatal(err)
	}
	clean, err := scope.Marker()
	if err != nil {
		t.Fatal(err)
	}
	isClean, err := clean.IsClean(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !isClean {
		t.Fatal("a scope did not read back its own clean marker")
	}
}

func TestScopeCloseIsSafeWithoutALedger(t *testing.T) {
	// Closing a scope whose ledger failed to open must not panic or report a
	// spurious error, so the shutdown path can call it unconditionally.
	scope := &Scope{}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
