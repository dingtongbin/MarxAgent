// SPDX-License-Identifier: Apache-2.0

// Package durability holds the crash tests. They are kept out of the package
// under test because the only honest way to crash a process is to be a separate
// process that stops existing: a panic recovered in the same process would still
// run its deferred flushes, which is precisely what a kill does not do.
package durability

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// The child process is this same test binary, re-entered through the standard
// helper process pattern so no second binary has to be built or shipped.
const (
	envRecords   = "MARXAGENT_CRASH_RECORDS"
	envFlushes   = "MARXAGENT_CRASH_FLUSHES"
	envScopeRoot = "MARXAGENT_CRASH_SCOPE"
	envSessionID = "MARXAGENT_CRASH_SESSION"
	envDone      = "MARXAGENT_CRASH_DONE"
)

// helperRecords is how many records the child wrote in total. Only the ones it
// flushed are allowed to survive, which is the promise the design makes.
const helperRecords = 40

func TestMain(m *testing.M) {
	if os.Getenv(envRecords) != "" {
		runHelperProcess()
		return
	}
	os.Exit(m.Run())
}

// runHelperProcess writes records into a scope and then stops existing without
// any cleanup, which is what an abrupt kill looks like from the data's point of
// view: whatever reached the disk stays, everything still in memory is gone.
func runHelperProcess() {
	records, err := strconv.Atoi(os.Getenv(envRecords))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad record count:", err)
		os.Exit(2)
	}
	flushes, err := strconv.Atoi(os.Getenv(envFlushes))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad flush count:", err)
		os.Exit(2)
	}
	root := os.Getenv(envScopeRoot)
	sessionID := os.Getenv(envSessionID)

	scope, err := storage.ProjectScope(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open scope:", err)
		os.Exit(2)
	}
	sessionJournal, eventsJournal, err := scope.OpenSessionJournals(sessionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open journals:", err)
		os.Exit(2)
	}
	// The marker starts dirty, exactly as a process that has not finished
	// shutting down would leave it.
	ctx := context.Background()
	marker, err := scope.Marker()
	if err != nil {
		fmt.Fprintln(os.Stderr, "marker:", err)
		os.Exit(2)
	}
	if err := marker.MarkDirty(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "mark dirty:", err)
		os.Exit(2)
	}
	buffer, err := storage.New(storage.Config{
		WatermarkRecords: 8,
		FlushPeriod:      time.Hour,
	}, sessionJournal, eventsJournal, scope.Audit())
	if err != nil {
		fmt.Fprintln(os.Stderr, "buffer:", err)
		os.Exit(2)
	}
	flushed := make([]int64, 0, flushes)
	for index := 1; index <= records; index++ {
		record := storage.Record{
			Stream: "session:" + sessionID,
			Type:   storage.RecordMessage,
			Data: json.RawMessage(fmt.Sprintf(
				`{"id":"m%d","role":"user","content":"record %d durability needle"}`, index, index)),
			Ts: time.Unix(int64(index), 0).UTC(),
		}
		if err := buffer.Append(record); err != nil {
			fmt.Fprintln(os.Stderr, "append:", err)
			os.Exit(2)
		}
		// Flush on a fixed cadence, so how many records reached the disk is
		// known exactly and the assertions can name a number.
		if index%8 == 0 && len(flushed) < flushes {
			if err := buffer.Flush(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "flush:", err)
				os.Exit(2)
			}
			flushed = append(flushed, int64(index))
		}
	}
	// Report how many records are allowed to be expected after the crash. The
	// file is a separate line so the parent reads it from stdout, not from a
	// pipe the child might not flush.
	report := map[string]any{
		"records":     records,
		"flushedUpTo": flushed,
		"buffered":    records - int(flushed[len(flushed)-1]),
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode report:", err)
		os.Exit(2)
	}
	if err := os.WriteFile(os.Getenv(envDone), encoded, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write report:", err)
		os.Exit(2)
	}
	// Stop dead. No Close, no flush, no clean marker. Anything the periodic loop
	// had not written is lost, exactly as it would be under a kill.
	os.Exit(9)
}

type crashReport struct {
	Records     int     `json:"records"`
	FlushedUpTo []int64 `json:"flushedUpTo"`
	Buffered    int     `json:"buffered"`
}

// runCrash starts the child, waits for it to stop and reads what it managed to
// flush.
func runCrash(t *testing.T, root string, flushes int) crashReport {
	t.Helper()
	reportPath := filepath.Join(t.TempDir(), "report.json")
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(),
		envRecords+"="+strconv.Itoa(helperRecords),
		envFlushes+"="+strconv.Itoa(flushes),
		envScopeRoot+"="+root,
		envSessionID+"=s1",
		envDone+"="+reportPath,
		"CGO_ENABLED=0",
	)
	// The child must not inherit the test timeout as its own lifetime.
	command.Env = append(command.Env, "MARXAGENT_CRASH=1")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("the child exited cleanly, so nothing was tested: %s", output)
	}
	var report crashReport
	raw, readErr := os.ReadFile(reportPath)
	if readErr != nil {
		t.Fatalf("the child died before it could report what it flushed: %v\n%s", readErr, output)
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

// TestRecordsAlreadyWrittenSurviveAKill is the core promise: a record that
// reached the disk is still readable after an abrupt stop, and nothing claims to
// be whole that is not.
func TestRecordsAlreadyWrittenSurviveAKill(t *testing.T) {
	root := t.TempDir()
	report := runCrash(t, root, 2)
	flushedThrough := report.FlushedUpTo[len(report.FlushedUpTo)-1]
	if flushedThrough <= 0 {
		t.Fatal("the child flushed nothing, so the test proves nothing")
	}

	// Before recovery the journal is read as it was left.
	sessionPath := filepath.Join(root, storage.ProjectDirName, "sessions", "s1", "session.jsonl")
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("the session journal is missing after the kill: %v", err)
	}

	scope, err := storage.ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	ctx := context.Background()

	marker, err := scope.Marker()
	if err != nil {
		t.Fatal(err)
	}
	clean, err := marker.IsClean(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Fatal("a killed process left a clean shutdown marker")
	}

	result, err := scope.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) == 0 {
		t.Fatalf("recovery found no journal: %#v", result)
	}

	// Zero loss for everything that was written, and no claim about the rest.
	records, partial, err := storage.ReadJournal(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 0 {
		t.Fatalf("a half written line survived: %q", partial)
	}
	if len(records) < int(flushedThrough) {
		t.Fatalf("records = %d, want at least the %d that were flushed",
			len(records), flushedThrough)
	}
	highest := int64(0)
	for _, record := range records {
		if record.Seq > highest {
			highest = record.Seq
		}
	}
	if highest < flushedThrough {
		t.Fatalf("highest sequence = %d, want at least %d", highest, flushedThrough)
	}

	// The recovered messages are searchable, and only the ones that were really
	// written are there.
	searched, err := scope.Audit().SearchMessages(ctx, "s1", "needle", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(searched) < int(flushedThrough) {
		t.Fatalf("searchable messages = %d, want at least %d", len(searched), flushedThrough)
	}
	if len(searched) > helperRecords {
		t.Fatalf("searchable messages = %d, more than were ever written", len(searched))
	}

	// Recovery is idempotent: a second pass changes nothing, which is what makes
	// it safe to run on every start that did not shut down cleanly.
	if err := scope.Checkpoint(ctx, "s1", highest, len(records), "after recovery"); err != nil {
		t.Fatal(err)
	}
	if err := scope.CheckpointStream(ctx, "events:s1", highest); err != nil {
		t.Fatal(err)
	}
	second, err := scope.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range second.Sessions {
		if session.Replayed != 0 {
			t.Fatalf("the second recovery replayed records again: %#v", session)
		}
	}
	again, err := scope.Audit().SearchMessages(ctx, "s1", "needle", 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(searched) {
		t.Fatalf("a second recovery changed the message count from %d to %d",
			len(searched), len(again))
	}
}

// TestJournalNeverEndsWithAPartialLine walks the truncation guarantee across
// several crash points, because a kill can land anywhere.
func TestJournalNeverEndsWithAPartialLine(t *testing.T) {
	for _, flushes := range []int{1, 2, 3} {
		t.Run("flushes="+strconv.Itoa(flushes), func(t *testing.T) {
			root := t.TempDir()
			runCrash(t, root, flushes)
			sessionPath := filepath.Join(root, storage.ProjectDirName, "sessions", "s1", "session.jsonl")
			records, partial, err := storage.ReadJournal(sessionPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(partial) != 0 {
				t.Fatalf("a half written line was left behind: %q", partial)
			}
			if len(records) == 0 {
				t.Fatal("nothing survived the kill")
			}
			scope, err := storage.ProjectScope(root)
			if err != nil {
				t.Fatal(err)
			}
			defer scope.Close()
			if _, err := scope.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			// After recovery the journal is back on a record boundary, so the
			// next append cannot produce a corrupt line of its own.
			afterRepair, partial, err := storage.ReadJournal(sessionPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(partial) != 0 {
				t.Fatalf("recovery left a partial line: %q", partial)
			}
			if len(afterRepair) < len(records) {
				t.Fatalf("recovery lost records: %d became %d", len(records), len(afterRepair))
			}
		})
	}
}

// TestAnInterruptedStreamIsMarkedIncomplete covers the other half of the
// promise: a half finished model response is kept, and marked, rather than
// presented as a complete one.
func TestAnInterruptedStreamIsMarkedIncomplete(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	scope, err := storage.ProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	sessionJournal, eventsJournal, err := scope.OpenSessionJournals("s1")
	if err != nil {
		t.Fatal(err)
	}
	// Both handles are closed, because leaving one open keeps the file locked on
	// Windows and the temporary directory could not be removed.
	defer sessionJournal.Close()
	// The record itself is intact, because the journal only ever writes whole
	// records. What was cut short is the model response inside it, which is why
	// the record exists at all.
	partial := storage.Record{
		Stream: "events:s1", Seq: 1, Type: storage.RecordPartialStream,
		Data: json.RawMessage(`{"text":"the answer was going to be forty two"}`),
		Ts:   time.Unix(1, 0).UTC(),
	}
	if err := eventsJournal.Write(ctx, []storage.Record{partial}); err != nil {
		t.Fatal(err)
	}
	if err := eventsJournal.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := scope.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, session := range result.Sessions {
		if session.Stream == "events:s1" && session.Incomplete == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the interrupted stream was not reported: %#v", result.Sessions)
	}
	records, _, err := storage.ReadJournal(filepath.Join(
		root, storage.ProjectDirName, "sessions", "s1", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	payload, ok := storage.IncompleteMessage(records[0])
	if !ok {
		t.Fatal("the interrupted record was not converted")
	}
	if !strings.Contains(string(payload), `"incomplete":true`) {
		t.Fatalf("payload = %s", payload)
	}
	if !strings.Contains(string(payload), "forty two") {
		t.Fatalf("the partial text was lost: %s", payload)
	}
	if strings.Contains(string(payload), `{"text"`) {
		t.Fatalf("the payload kept its raw json scaffolding: %s", payload)
	}
}
