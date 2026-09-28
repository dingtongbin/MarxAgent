// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// SplitSink routes each record to the journal its own stream belongs to, and
// hands every record to the ledger.
//
// It exists because one durability pipeline serves more than one stream, and
// those streams do not live in the same file. A session's message history and
// its raw event stream are two files a person can read with `cat`, and a
// pipeline that wrote both into one would be a pipeline that made both of them
// unreadable for the reason somebody wanted them readable.
//
// The ledger is not split, and deliberately: the ledger is the audited copy of
// everything that happened, and a record missing from it because it belonged to
// a particular stream would be a record the audit could not show.
type SplitSink struct {
	mu sync.Mutex
	// routes maps a stream kind onto the journal that stream is written to.
	routes map[string]Sink
	// ledger receives every record, and may be nil where there is no ledger.
	ledger Sink
	// indexable turns a record into the shape the ledger indexes, where the two
	// cannot be the same shape. It is nil when they are.
	//
	// It exists because the two copies answer different questions. A journal is
	// read back to rebuild a conversation, so it holds the message as it was; the
	// ledger is searched, so it holds the message as text a full text index can
	// take. One payload cannot be both, and neither copy being derived from the
	// other at read time would be safe: the journal is the copy that has to
	// survive whatever happens to the database.
	indexable func(Record) (Record, error)
	// unrouted counts the records whose stream named no journal, so a stream
	// written nowhere is a number rather than a silence.
	unrouted int
}

// NewSplitSink builds a split sink. A route with a nil sink is refused rather
// than ignored, because a journal that silently receives nothing is a
// conversation that appears to work and is not on disk.
//
// indexable may be nil, which means the ledger is given the same payload the
// journals are.
func NewSplitSink(routes map[string]Sink, ledger Sink, indexable func(Record) (Record, error)) (*SplitSink, error) {
	for kind, sink := range routes {
		if strings.TrimSpace(kind) == "" {
			return nil, fmt.Errorf("storage: a journal route must have a stream kind")
		}
		if sink == nil {
			return nil, fmt.Errorf("storage: journal route %q has no sink", kind)
		}
	}
	return &SplitSink{routes: routes, ledger: ledger, indexable: indexable}, nil
}

// Write partitions a batch by stream kind and writes each part with one call.
//
// The partition is built before anything is written, so a batch is either
// written whole or reported as a failure: a batch half written is a conversation
// whose recovery has to decide what the missing half was, and that decision is
// exactly what a journal is supposed to remove.
func (s *SplitSink) Write(ctx context.Context, batch []Record) error {
	if len(batch) == 0 {
		return nil
	}
	partitions := make(map[string][]Record, len(s.routes))
	var order []string
	unrouted := make([]Record, 0)
	for _, record := range batch {
		kind := StreamKind(record.Stream)
		if _, routed := s.routes[kind]; !routed {
			unrouted = append(unrouted, record)
			continue
		}
		if _, seen := partitions[kind]; !seen {
			order = append(order, kind)
		}
		partitions[kind] = append(partitions[kind], record)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.unrouted += len(unrouted)

	sort.Strings(order)
	for _, kind := range order {
		if err := s.routes[kind].Write(ctx, partitions[kind]); err != nil {
			return fmt.Errorf("storage: journal %q: %w", kind, err)
		}
	}
	if s.ledger != nil {
		// The ledger's copy is derived, and a record that cannot be turned into it
		// is a failure rather than a record quietly left out: a ledger missing a
		// message is a ledger that cannot answer "what was said in this session".
		indexed := make([]Record, 0, len(batch))
		for _, record := range batch {
			if s.indexable == nil {
				indexed = append(indexed, record)
				continue
			}
			converted, err := s.indexable(record)
			if err != nil {
				return err
			}
			indexed = append(indexed, converted)
		}
		if err := s.ledger.Write(ctx, indexed); err != nil {
			return fmt.Errorf("storage: ledger: %w", err)
		}
	}
	return nil
}

// Sync makes every journal and the ledger durable.
func (s *SplitSink) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for kind, sink := range s.routes {
		if err := sink.Sync(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("storage: journal %q: %w", kind, err)
		}
	}
	if s.ledger != nil {
		if err := s.ledger.Sync(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("storage: ledger: %w", err)
		}
	}
	return firstErr
}

// Unrouted reports how many records named a stream with no journal behind it.
func (s *SplitSink) Unrouted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unrouted
}

// StreamKind is the part of a stream name before the colon, which is what says
// which journal a record belongs in: "session:abc" is the message history of a
// session and "events:abc" is that session's raw event stream.
//
// The split is at the first colon rather than the last because a stream name is
// written as kind then identifier, and an identifier is allowed to contain
// colons while a kind is not.
func StreamKind(stream string) string {
	kind, _, found := strings.Cut(stream, ":")
	if !found {
		return stream
	}
	return kind
}

var _ Sink = (*SplitSink)(nil)
