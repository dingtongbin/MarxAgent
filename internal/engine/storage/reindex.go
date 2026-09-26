// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"fmt"
)

// ReindexReport says what a rebuild did.
type ReindexReport struct {
	// Streams read, ordered, so two runs report the same thing.
	Streams []string `json:"streams"`
	// Records read from the journals.
	Records int `json:"records"`
	// Records written to the ledger.
	Indexed int `json:"indexed"`
	// Records dropped because they carried no type or stream, or had a
	// negative sequence.
	Skipped int `json:"skipped"`
	// Trailing half written lines removed from the journals.
	TruncatedBytes int `json:"truncated_bytes"`
}

// Reindex rebuilds the ledger from the journals.
//
// The journals are the human readable copy and the ledger is the queryable one,
// so this is what makes a damaged database recoverable without asking anyone for
// a backup. The journals win, because they are the copy written first and the
// one that survives a crash the ledger may not.
//
// It is a repair tool, so it replaces the ledger rather than merging into it.
// Running it twice on the same journals produces the same database, which is the
// property that makes it safe to offer a user who is already worried.
func Reindex(ctx context.Context, root string, audit *AuditSink) (ReindexReport, error) {
	report := ReindexReport{}
	if audit == nil {
		return report, fmt.Errorf("storage: reindex needs a ledger")
	}
	records, report, err := collectJournalRecords(root, &report)
	if err != nil {
		return report, err
	}
	if err := audit.rebuild(ctx, records); err != nil {
		return report, err
	}
	report.Indexed = len(records)
	return report, nil
}

// Verify reports what the journals hold and what the ledger holds, so a query
// command can tell a user whether the two agree before offering to rebuild.
func (s *AuditSink) Verify(ctx context.Context, root string) (ReindexReport, error) {
	report := ReindexReport{}
	records, report, err := collectJournalRecords(root, &report)
	if err != nil {
		return report, err
	}
	ledgerCount, err := s.totalEvents(ctx)
	if err != nil {
		return report, err
	}
	report.Records = len(records) + report.Skipped
	report.Indexed = ledgerCount
	return report, nil
}

// collectJournalRecords reads every journal in a scope, repairing a trailing
// half written line on the way, because a rebuild is the one moment where
// truncating a remnant is unambiguously the right thing to do.
func collectJournalRecords(root string, report *ReindexReport) ([]Record, ReindexReport, error) {
	journals, err := discoverJournals(root)
	if err != nil {
		return nil, *report, err
	}
	report.Streams = make([]string, 0, len(journals))
	var all []Record
	for _, journal := range journals {
		report.Streams = append(report.Streams, journal.stream)
		records, partial, err := ReadJournal(journal.path)
		if err != nil {
			return nil, *report, err
		}
		if len(partial) > 0 {
			if err := TruncatePartialJournal(journal.path, partial); err != nil {
				return nil, *report, err
			}
			report.TruncatedBytes += len(partial)
		}
		report.Records += len(records)
		for _, record := range records {
			if record.Type == "" || record.Stream == "" || record.Seq < 0 {
				report.Skipped++
				continue
			}
			all = append(all, record)
		}
	}
	return all, *report, nil
}

// rebuild replaces the contents of the ledger in two steps: the old contents are
// cleared in one transaction, then the records are written. A failure before the
// commit leaves the previous contents in place rather than half a database.
func (s *AuditSink) rebuild(ctx context.Context, records []Record) error {
	if err := s.clear(ctx); err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	return s.Write(ctx, records)
}

func (s *AuditSink) clear(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrBufferClosed
	}
	transaction, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin reindex transaction: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	// The search index follows messages through triggers, so emptying the table
	// empties the index with it and no separate index cleanup can be forgotten.
	for _, table := range []string{"events", "tool_calls", "tool_call_audit", "messages"} {
		if _, err := transaction.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("storage: clear %s: %w", table, err)
		}
	}
	if _, err := transaction.ExecContext(ctx,
		`DELETE FROM meta WHERE key LIKE 'checkpoint%'`); err != nil {
		return fmt.Errorf("storage: clear checkpoints: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("storage: commit reindex clear: %w", err)
	}
	return nil
}

func (s *AuditSink) totalEvents(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&count); err != nil {
		return 0, fmt.Errorf("storage: count ledger events: %w", err)
	}
	return count, nil
}

// schemaVersionDrift reports a database written by a different schema version,
// which is the case where a rebuild is unsafe because the journals would be read
// into a shape the code no longer understands.
func (s *AuditSink) schemaVersionDrift(ctx context.Context) (bool, error) {
	version, err := s.SchemaVersionOf()
	if err != nil {
		return false, err
	}
	return version != SchemaVersion, nil
}

// ErrSchemaDrift is returned when a ledger was written by another schema.
var ErrSchemaDrift = fmt.Errorf("storage: the ledger was written by another schema version")

// CheckRebuildable reports whether a rebuild can run against this ledger.
func (s *AuditSink) CheckRebuildable(ctx context.Context) error {
	drift, err := s.schemaVersionDrift(ctx)
	if err != nil {
		return err
	}
	if drift {
		return fmt.Errorf("%w: rebuild is not safe", ErrSchemaDrift)
	}
	return nil
}
