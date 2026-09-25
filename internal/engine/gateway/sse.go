// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
)

// MaxStreamLineBytes bounds one server-sent-events line so a hostile or broken
// endpoint cannot exhaust memory.
const MaxStreamLineBytes = 1 << 20

// MaxErrorBodyBytes bounds how much of a failed response body is reported.
const MaxErrorBodyBytes = 4 << 10

// StreamEvent is one server-sent event.
type StreamEvent struct {
	Name string
	Data string
	ID   string
}

// DecodeStream parses a server-sent-events body. Comments, retry hints and
// unknown fields are ignored, multi-line data is joined with newlines and a
// trailing event without a blank line terminator is still delivered.
func DecodeStream(body io.Reader) iter.Seq[StreamEvent] {
	return func(yield func(StreamEvent) bool) {
		_ = decodeStream(body, func(event StreamEvent) error {
			if !yield(event) {
				return errStopDecoding
			}
			return nil
		})
	}
}

var errStopDecoding = errors.New("gateway: stop decoding stream")

func decodeStream(body io.Reader, yield func(StreamEvent) error) error {
	if body == nil {
		return errors.New("gateway: stream body must not be nil")
	}
	reader := bufio.NewReaderSize(body, 64<<10)
	event := StreamEvent{}
	var data []string
	hasData := false
	dispatch := func() error {
		// A blank line always ends the event, even when no data was collected,
		// so the event name never leaks into the next event.
		event.Data = ""
		if !hasData {
			event = StreamEvent{}
			return nil
		}
		event.Data = strings.Join(data, "\n")
		data = data[:0]
		hasData = false
		if err := yield(event); err != nil {
			return err
		}
		event = StreamEvent{}
		return nil
	}
	for {
		line, err := readStreamLine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if line == "" {
			if dispatchErr := dispatch(); dispatchErr != nil {
				if errors.Is(dispatchErr, errStopDecoding) {
					return nil
				}
				return dispatchErr
			}
		} else if field, value, ok := parseStreamField(line); ok {
			switch field {
			case "event":
				event.Name = value
			case "data":
				data = append(data, value)
				hasData = true
			case "id":
				event.ID = value
			}
		}
		if err != nil {
			if dispatchErr := dispatch(); dispatchErr != nil {
				if errors.Is(dispatchErr, errStopDecoding) {
					return nil
				}
				return dispatchErr
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func readStreamLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		if len(line) > MaxStreamLineBytes {
			return "", fmt.Errorf("gateway: stream line exceeds %d bytes", MaxStreamLineBytes)
		}
		return trimLineEnding(line), err
	}
	if len(line) > MaxStreamLineBytes {
		return "", fmt.Errorf("gateway: stream line exceeds %d bytes", MaxStreamLineBytes)
	}
	return trimLineEnding(line), nil
}

func trimLineEnding(line string) string {
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r")
}

func parseStreamField(line string) (field string, value string, ok bool) {
	if line == "" || strings.HasPrefix(line, ":") {
		return "", "", false
	}
	name, rest, found := strings.Cut(line, ":")
	if !found {
		return "", "", false
	}
	return name, strings.TrimPrefix(rest, " "), true
}

// ReadErrorBody reads a bounded prefix of a failed response body.
func ReadErrorBody(body io.Reader) string {
	if body == nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(body, MaxErrorBodyBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// StreamWriter emits server-sent events. It exists so tests and local mock
// servers can produce byte-identical provider corpora.
type StreamWriter struct {
	writer io.Writer
}

// NewStreamWriter wraps a writer.
func NewStreamWriter(writer io.Writer) *StreamWriter {
	return &StreamWriter{writer: writer}
}

// WriteEvent writes one named event with a JSON payload.
func (w *StreamWriter) WriteEvent(name string, payload any) error {
	if w == nil || w.writer == nil {
		return errors.New("gateway: stream writer is not initialized")
	}
	encoded, err := marshalPayload(payload)
	if err != nil {
		return err
	}
	if name != "" {
		if _, err := fmt.Fprintf(w.writer, "event: %s\n", name); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(string(encoded), "\n") {
		if _, err := fmt.Fprintf(w.writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w.writer, "\n")
	return err
}

// WriteRawData writes a data line verbatim. Providers use it for non-JSON
// payloads such as the OpenAI "[DONE]" sentinel.
func (w *StreamWriter) WriteRawData(data string) error {
	if w == nil || w.writer == nil {
		return errors.New("gateway: stream writer is not initialized")
	}
	if _, err := fmt.Fprintf(w.writer, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}

// WriteComment writes a comment line, which providers use as a keep-alive.
func (w *StreamWriter) WriteComment(text string) error {
	if w == nil || w.writer == nil {
		return errors.New("gateway: stream writer is not initialized")
	}
	_, err := fmt.Fprintf(w.writer, ": %s\n\n", text)
	return err
}

// Flush pushes buffered events to the client. Real endpoints flush per event so
// the first token is not stuck in a buffer.
func (w *StreamWriter) Flush() error {
	if w == nil || w.writer == nil {
		return errors.New("gateway: stream writer is not initialized")
	}
	flusher, ok := w.writer.(interface{ Flush() })
	if !ok {
		return nil
	}
	flusher.Flush()
	return nil
}
