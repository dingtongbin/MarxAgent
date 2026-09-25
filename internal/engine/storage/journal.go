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

// JournalPaths maps a stream name onto its journal file name. Streams are
// grouped by prefix so a session and its events land in predictable files.
func JournalPaths(root string, streams []string) (map[string]string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("storage: journal root must not be empty")
	}
	paths := make(map[string]string, len(streams))
	for _, stream := range streams {
		name, err := journalFileName(stream)
		if err != nil {
			return nil, err
		}
		paths[stream] = filepath.Join(root, name)
	}
	return paths, nil
}

func journalFileName(stream string) (string, error) {
	kind, id, found := strings.Cut(stream, ":")
	if !found || kind == "" || id == "" {
		return "", fmt.Errorf("storage: stream %q must be <kind>:<id>", stream)
	}
	if strings.ContainsAny(id, `/\:`) {
		return "", fmt.Errorf("storage: stream id %q contains a path separator", id)
	}
	return kind + "-" + id + ".jsonl", nil
}

// SortedStreams returns stream names in a stable order.
func SortedStreams(streams map[string]string) []string {
	names := make([]string, 0, len(streams))
	for name := range streams {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func trimLineEnd(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}
