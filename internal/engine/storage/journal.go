// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// JournalStreams are the stream kinds the pipeline writes. Recovery uses the
// list to tell a journal apart from an unrelated file.
const (
	StreamSession  = "session"
	StreamEvents   = "events"
	StreamSubAgent = "subagent"
	StreamBroker   = "broker"
)

// JournalFileName maps a stream onto its file inside a scope root.
func JournalFileName(root, stream string) (string, error) {
	return journalPath(root, stream)
}

// JournalSink is the human readable half of the durability design: one JSON
// object per line, append only, so a session can be read with cat, less or jq
// and rebuilt without the database.
type JournalSink struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	closed bool
}

// OpenJournal creates or opens a journal for appending.
func OpenJournal(path string) (*JournalSink, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("storage: journal path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("storage: create journal directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("storage: open journal: %w", err)
	}
	return &JournalSink{file: file, writer: bufio.NewWriterSize(file, 64<<10)}, nil
}

// Write appends a batch with a single write call so the syscall count stays flat.
func (s *JournalSink) Write(_ context.Context, batch []Record) error {
	if len(batch) == 0 {
		return nil
	}
	var builder strings.Builder
	for _, record := range batch {
		encoded, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("storage: encode journal record: %w", err)
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrBufferClosed
	}
	if _, err := s.writer.WriteString(builder.String()); err != nil {
		return fmt.Errorf("storage: append journal: %w", err)
	}
	return s.writer.Flush()
}

// Sync makes the appended records durable.
func (s *JournalSink) Sync(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrBufferClosed
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("storage: sync journal: %w", err)
	}
	return nil
}

// Close flushes and closes the journal.
func (s *JournalSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.writer.Flush()
	if syncErr := s.file.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := s.file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// ReadJournal parses a journal file. A trailing partial line is what a crash
// leaves behind, and it is reported separately so recovery can truncate it
// instead of treating it as data.
func ReadJournal(path string) (records []Record, partial []byte, err error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("storage: open journal: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()

	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		line, readErr := reader.ReadBytes('\n')
		trimmed := trimLineEnd(line)
		if len(trimmed) > 0 {
			var record Record
			if unmarshalErr := json.Unmarshal(trimmed, &record); unmarshalErr != nil {
				// A line that will not parse is a crash remnant, never data.
				partial = append(partial, trimmed...)
				return records, partial, nil
			}
			records = append(records, record)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return records, partial, nil
			}
			return records, partial, fmt.Errorf("storage: read journal: %w", readErr)
		}
	}
}

// TruncatePartialJournal removes a trailing partial line so the next append
// starts on a record boundary.
func TruncatePartialJournal(path string, partial []byte) error {
	if len(partial) == 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("storage: stat journal: %w", err)
	}
	size := info.Size() - int64(len(partial))
	if size < 0 {
		return fmt.Errorf("storage: journal is shorter than its partial line")
	}
	if err := os.Truncate(path, size); err != nil {
		return fmt.Errorf("storage: truncate journal: %w", err)
	}
	return nil
}

// journalPath maps a stream onto its file inside a scope, following the layout
// the design specifies: session streams live in sessions/<id>/<kind>.jsonl and
// the broker stream has its own directory.
func journalPath(root, stream string) (string, error) {
	kind, id, found := strings.Cut(stream, ":")
	if !found || kind == "" || id == "" {
		return "", fmt.Errorf("storage: stream %q must be <kind>:<id>", stream)
	}
	if err := ValidateID("stream id", id); err != nil {
		return "", err
	}
	if !knownStreamKind(kind) {
		return "", fmt.Errorf("storage: unknown stream kind %q", kind)
	}
	if kind == StreamBroker {
		return filepath.Join(root, BrokerDirName, BrokerJournalName+".jsonl"), nil
	}
	return filepath.Join(root, SessionsDirName, id, kind+".jsonl"), nil
}

func knownStreamKind(kind string) bool {
	switch kind {
	case StreamSession, StreamEvents, StreamSubAgent, StreamBroker, "recovery":
		return true
	default:
		return false
	}
}

// journalFile is one discovered journal and the stream it holds.
type journalFile struct {
	path   string
	stream string
}

// discoverJournals walks a scope and reports every journal it holds. The stream
// comes from the layout, not from a guess: the file name gives the kind and the
// containing directory gives the identifier, so an unrelated file in the scope
// is never mistaken for a journal.
func discoverJournals(root string) ([]journalFile, error) {
	// A missing scope root is a caller error, while a missing journal file is
	// the normal first run, so the root is checked before the walk starts.
	if info, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("storage: read scope root: %w", err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("storage: scope root %q is not a directory", root)
	}
	var found []journalFile
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A journal removed while the walk runs is not a failure; the next
			// recovery pass will simply not see it.
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if strings.HasSuffix(entry.Name(), ".jsonl") && looksLikeJournal(entry.Name(), filepath.Base(filepath.Dir(path))) {
			// A directory sitting where a journal belongs is a damaged layout.
			// Skipping it would let a rebuild report success while quietly
			// leaving a stream out, which is the opposite of what a recovery tool
			// is for.
			if entry.IsDir() {
				return fmt.Errorf("storage: %s is a directory, not a journal", path)
			}
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		kind := strings.TrimSuffix(entry.Name(), ".jsonl")
		directory := filepath.Base(filepath.Dir(path))
		id := directory
		// A session or event journal is named after its kind. The broker journal
		// is named journal.jsonl inside a broker directory instead, so the
		// directory can carry the kind. The file name is tried first, which keeps
		// a session whose identifier happens to be a stream kind classified by
		// its own name.
		if !knownStreamKind(kind) {
			kind, id = directory, strings.TrimSuffix(entry.Name(), ".jsonl")
		}
		if !knownStreamKind(kind) {
			return nil
		}
		if err := ValidateID("stream id", id); err != nil {
			return nil
		}
		found = append(found, journalFile{path: path, stream: kind + ":" + id})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("storage: walk scope: %w", err)
	}
	sort.Slice(found, func(first, second int) bool {
		return found[first].path < found[second].path
	})
	return found, nil
}

// looksLikeJournal reports whether a name would be a journal, given the
// directory holding it. It is the same rule discoverJournals applies, factored
// out so the damaged layout check and the discovery agree.
func looksLikeJournal(name, directory string) bool {
	if !strings.HasSuffix(name, ".jsonl") {
		return false
	}
	base := strings.TrimSuffix(name, ".jsonl")
	if knownStreamKind(base) {
		return true
	}
	return knownStreamKind(directory)
}

func trimLineEnd(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}
