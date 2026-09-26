// SPDX-License-Identifier: Apache-2.0

// Package lsp holds the language server client. The server runs as a separate
// process speaking JSON-RPC over its standard streams, and the core loop only
// ever sees a tool, so nothing here leaks a transport shape into the agent.
package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// ProtocolVersion is the language server protocol version this client speaks.
const ProtocolVersion = "3.17"

// ContentLengthHeader is the header the protocol frames with. The header is
// spelled out rather than inferred, because a server that omits it and a server
// that spells it slightly differently both appear in the wild and both mean the
// stream is no longer framed.
const ContentLengthHeader = "Content-Length"

// ID is a request identifier. The protocol allows a number or a string, so both
// are carried and the raw form is kept for the wire.
type ID struct {
	// Number is set for a numeric identifier.
	Number int64
	// Text is set for a string identifier.
	Text string
}

// IsZero reports whether the identifier is unset.
func (i ID) IsZero() bool { return i.Number == 0 && i.Text == "" }

// String renders the identifier for a log line.
func (i ID) String() string {
	if i.Text != "" {
		return strconv.Quote(i.Text)
	}
	return strconv.FormatInt(i.Number, 10)
}

// MarshalJSON writes a number when the identifier is numeric and a string
// otherwise, because a server that receives a number where it sent a string will
// not correlate the response.
func (i ID) MarshalJSON() ([]byte, error) {
	if i.Text != "" {
		return json.Marshal(i.Text)
	}
	return json.Marshal(i.Number)
}

// UnmarshalJSON accepts either form.
func (i *ID) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &i.Text)
	}
	return json.Unmarshal(data, &i.Number)
}

// Message is one framed protocol message. A request carries an identifier and a
// method, a response an identifier and a result, and a notification a method with
// no identifier at all.
type Message struct {
	// JSONRPC is always the two literal string, and it is checked on the way in
	// because a stream that has lost sync produces plausible looking messages.
	JSONRPC string `json:"jsonrpc"`
	// ID is set for requests and responses, unset for notifications.
	ID ID `json:"id,omitempty"`
	// Method is set for requests and notifications.
	Method string `json:"method,omitempty"`
	// Params carries the payload. It stays raw because a server is free to add
	// fields and this client must not drop the ones it does not know.
	Params json.RawMessage `json:"params,omitempty"`
	// Result is the payload of a successful response.
	Result json.RawMessage `json:"result,omitempty"`
	// Error is the payload of a failed response.
	Error *ResponseError `json:"error,omitempty"`
}

// IsRequest reports whether the message asks for something.
func (m Message) IsRequest() bool { return m.Method != "" && !m.ID.IsZero() }

// IsNotification reports whether the message announces something.
func (m Message) IsNotification() bool { return m.Method != "" && m.ID.IsZero() }

// IsResponse reports whether the message answers something.
func (m Message) IsResponse() bool { return m.Method == "" && !m.ID.IsZero() }

// ResponseError is a failed response.
type ResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface with the code in the text, because a
// caller deciding what to do about a failure needs the code and a bare sentence
// loses it.
func (e *ResponseError) Error() string {
	if e == nil {
		return "no error"
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("lsp error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message)
}

// The error codes the protocol defines that this client reacts to by name. A
// server may define its own, and those are reported by number instead.
const (
	// CodeMethodNotFound means the server does not implement the method, which for
	// a language without a server is the normal answer rather than a failure.
	CodeMethodNotFound = -32601
	// CodeInvalidParams means the request was malformed.
	CodeInvalidParams = -32602
	// CodeInternalError means the server failed while handling it.
	CodeInternalError = -32603
	// CodeRequestCancelled means the server dropped the request.
	CodeRequestCancelled = -32800
	// CodeContentModified means the document changed under the request, so its
	// answer describes a version that no longer exists.
	CodeContentModified = -32801
)

// Codes named here are ones the client acts on rather than merely reports.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
)

// Notification is a message the server pushes without being asked.
type Notification struct {
	// Method is the notification name.
	Method string
	// Params is its payload.
	Params json.RawMessage
}

// Request is a message the server sends to this client, which has to be answered
// or the server waits forever.
type Request struct {
	ID     ID
	Method string
	Params json.RawMessage
}

// Direction says which side a handler belongs to, which is what lets one
// dispatcher serve both directions.
type Direction int

const (
	// DirectionOutbound is a message this client sent.
	DirectionOutbound Direction = iota
	// DirectionInbound is a message the server sent.
	DirectionInbound
)

// Handler deals with one inbound message. It returns a response to send, or nil
// for a message that needs no answer.
type Handler interface {
	Handle(ctx Context, message Message) (Message, bool)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx Context, message Message) (Message, bool)

// Handle calls the function.
func (f HandlerFunc) Handle(ctx Context, message Message) (Message, bool) { return f(ctx, message) }

// Context is the per connection state a handler may read. It is an interface so
// a handler can be tested without a connection.
type Context interface {
	// Direction says which side sent the message.
	Direction() Direction
	// Request returns the request, when the message is one.
	Request() (Request, bool)
	// Notification returns the notification, when the message is one.
	Notification() (Notification, bool)
	// Response returns the response, when the message is one.
	Response() (Message, bool)
}

// Reader frames messages off a stream.
type Reader struct {
	reader *bufio.Reader
	// limit bounds one message, so a server that announces a huge length cannot
	// make this client allocate without limit.
	limit int64
}

// DefaultReadLimit bounds a single message. The protocol's own messages are small,
// and a language server announcing gigabytes is either broken or hostile.
const DefaultReadLimit = 64 << 20

// NewReader builds a reader.
func NewReader(stream io.Reader) *Reader {
	return &Reader{
		reader: bufio.NewReaderSize(stream, 64<<10),
		limit:  DefaultReadLimit,
	}
}

// SetLimit changes the per message bound.
func (r *Reader) SetLimit(limit int64) {
	if limit > 0 {
		r.limit = limit
	}
}

// Read returns the next message.
func (r *Reader) Read() (Message, error) {
	length := int64(-1)
	contentType := ""
	for {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			if err == io.EOF && strings.TrimSpace(line) == "" {
				return Message{}, io.EOF
			}
			return Message{}, fmt.Errorf("lsp: read a header: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			// The blank line ends the headers, which is where the content begins.
			break
		}
		name, value, found := strings.Cut(trimmed, ":")
		if !found {
			return Message{}, fmt.Errorf("lsp: malformed header %q", trimmed)
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case strings.ToLower(ContentLengthHeader):
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return Message{}, fmt.Errorf("lsp: the content length %q is not a number", value)
			}
			length = parsed
		case "content-type":
			contentType = strings.ToLower(strings.TrimSpace(value))
		}
		// An unknown header is ignored rather than treated as an error, because the
		// protocol allows the headers to grow and refusing to read would break the
		// client against a newer server for no benefit.
	}
	if length < 0 {
		return Message{}, fmt.Errorf("lsp: the message had no %s header", ContentLengthHeader)
	}
	if length > r.limit {
		return Message{}, fmt.Errorf("lsp: the message announced %d bytes, over the %d limit",
			length, r.limit)
	}
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") &&
		!strings.HasPrefix(contentType, "application/vscode-jsonrpc") {
		return Message{}, fmt.Errorf("lsp: unsupported content type %q", contentType)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r.reader, payload); err != nil {
		return Message{}, fmt.Errorf("lsp: read the body: %w", err)
	}
	var message Message
	if err := json.Unmarshal(payload, &message); err != nil {
		// A body that will not parse means the stream has lost sync, so the caller
		// has to restart rather than keep reading from a stream it can no longer
		// trust.
		return Message{}, fmt.Errorf("lsp: decode the body: %w", err)
	}
	if message.JSONRPC != "" && message.JSONRPC != "2.0" {
		return Message{}, fmt.Errorf("lsp: the message claims protocol %q", message.JSONRPC)
	}
	return message, nil
}

// Writer frames messages onto a stream.
type Writer struct {
	mu     sync.Mutex
	writer io.Writer
	buffer bytes.Buffer
}

// NewWriter builds a writer.
func NewWriter(stream io.Writer) *Writer { return &Writer{writer: stream} }

// Write frames and sends one message.
func (w *Writer) Write(message Message) error {
	if message.JSONRPC == "" {
		message.JSONRPC = "2.0"
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("lsp: encode the message: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// The whole frame is assembled before anything is written, so a partial write
	// cannot leave a header without a body.
	w.buffer.Reset()
	fmt.Fprintf(&w.buffer, "%s: %d\r\n\r\n", ContentLengthHeader, len(payload))
	w.buffer.Write(payload)
	if _, err := w.writer.Write(w.buffer.Bytes()); err != nil {
		return fmt.Errorf("lsp: write the message: %w", err)
	}
	return nil
}
