// SPDX-License-Identifier: Apache-2.0

package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
)

// ErrNotRunning is returned when a request is made against a server that is not
// up and could not be started.
var ErrNotRunning = errors.New("lsp: the language server is not running")

// ErrRestarting is returned while a crashed server is being brought back.
var ErrRestarting = errors.New("lsp: the language server is restarting")

// Starter launches the server. It is the sandbox's runner, so a mode that confines
// processes confines the language server with them and there is exactly one
// process abstraction in the project.
type Starter = sandbox.Runner

// Config configures a client.
type Config struct {
	// Command is the server to run, argv with the program first.
	Command []string
	// Workspace is the root the server is opened on.
	Workspace string
	// Starter launches the process. A nil starter means no confinement, which is
	// what a mode with no sandbox uses.
	Starter Starter
	// Environment is what the server sees. A nil value gives it an empty
	// environment, because a server that inherits everything is a server nobody
	// configured.
	Environment []string
	// RequestTimeout bounds one request. Zero selects the default.
	RequestTimeout time.Duration
	// StartTimeout bounds the initialize handshake. Zero selects the default.
	StartTimeout time.Duration
	// MaxRestarts bounds how many times a crashed server is brought back before the
	// client gives up. Zero selects the default. A server that crashes in a loop
	// would otherwise restart forever, which is worse than being down.
	MaxRestarts int
	// RestartBackoff is the pause before a restart. The first restart is immediate,
	// because a crash during a request is usually a transient.
	RestartBackoff time.Duration
	// Trace records the traffic, for a support question. A nil value discards it.
	Trace func(direction Direction, message Message)
}

const (
	// DefaultRequestTimeout bounds one request.
	DefaultRequestTimeout = 30 * time.Second
	// DefaultStartTimeout bounds the initialize handshake.
	DefaultStartTimeout = 60 * time.Second
	// DefaultMaxRestarts is how many times a crashed server comes back.
	DefaultMaxRestarts = 3
	// DefaultRestartBackoff is the pause before a restart.
	DefaultRestartBackoff = 250 * time.Millisecond
)

// pending is one in flight request.
type pending struct {
	response chan Message
	// done is closed when the request is abandoned, so a late response does not
	// block a reader forever.
	done chan struct{}
	once sync.Once
}

func (p *pending) finish() {
	p.once.Do(func() { close(p.done) })
}

// abandoned reports whether the request was given up on.
func (p *pending) abandoned() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// Client is a language server connection.
//
// It starts lazily: no process exists until something asks a question, because a
// language server is expensive to hold and a session that never asks one should
// not pay for it.
type Client struct {
	config Config

	mu sync.Mutex
	// process is the running server, nil when it has never started or has stopped.
	process sandbox.Process
	// reader and writer frame the stream.
	reader *Reader
	writer *Writer
	// running is the goroutine draining the stream.
	running chan struct{}
	// readerGeneration is the launch this reader belongs to.
	readerGeneration int64
	// stderr is drained separately, because a server that fills its error pipe
	// while nobody reads it blocks forever.
	stderrDrained chan struct{}
	// nextID hands out request identifiers.
	nextID int64
	// pending correlates responses to requests.
	pending map[string]*pending
	// initialized is true once the handshake finished, which is what a request needs
	// before it is sent.
	initialized bool
	// capabilities is what the server said it supports, so a request it does not
	// implement is not sent at all.
	capabilities ServerCapabilities
	// openDocuments are the files the server has been told about, because the
	// protocol answers questions about a document it has been given.
	openDocuments map[string]bool
	// restarts counts the restarts used, against the bound.
	restarts int
	// stopped records a deliberate shutdown, so a read loop ending is not treated
	// as a crash.
	stopped bool
	// starting guards against two callers starting the same server.
	starting bool
	// retriedMethods marks the methods already retried after a crash, so a question
	// is asked twice at most.
	retriedMethods map[string]bool
	// generation counts launches. A reader whose generation is no longer current
	// belongs to a server that has already been replaced, and it must not touch the
	// live connection's state.
	generation int64
	// crash records the last crash, so a caller can report why the server is down.
	crash atomic.Pointer[Crash]
	// lastReadError records why a reader ended, so a support question can be
	// answered without reproducing it.
	lastReadError atomic.Pointer[string]

	// diagnostics is the store the publishDiagnostics notifications feed.
	diagnostics *DiagnosticStore
	// handlers deal with the requests the server sends back.
	handlers []Handler
}

// Crash describes why a server went away.
type Crash struct {
	// At is when it was noticed.
	At time.Time
	// Err is what the process reported.
	Err error
	// Restarts is how many restarts had already been used.
	Restarts int
}

// Error implements the error interface.
func (c *Crash) Error() string {
	if c == nil {
		return "no crash"
	}
	return fmt.Sprintf("the language server exited after %d restarts: %v", c.Restarts, c.Err)
}

// New builds a client.
func New(config Config) (*Client, error) {
	if len(config.Command) == 0 || strings.TrimSpace(config.Command[0]) == "" {
		return nil, fmt.Errorf("lsp: a client needs a command to run")
	}
	if strings.TrimSpace(config.Workspace) == "" {
		return nil, fmt.Errorf("lsp: a client needs a workspace")
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = DefaultRequestTimeout
	}
	if config.StartTimeout <= 0 {
		config.StartTimeout = DefaultStartTimeout
	}
	if config.MaxRestarts <= 0 {
		config.MaxRestarts = DefaultMaxRestarts
	}
	if config.RestartBackoff < 0 {
		config.RestartBackoff = DefaultRestartBackoff
	}
	return &Client{
		config:        config,
		pending:       map[string]*pending{},
		openDocuments: map[string]bool{},
		diagnostics:   NewDiagnosticStore(),
	}, nil
}

// Diagnostics exposes the store the publishDiagnostics notifications feed.
func (c *Client) Diagnostics() *DiagnosticStore { return c.diagnostics }

// Capabilities reports what the server said it supports.
func (c *Client) Capabilities() ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capabilities
}

// Running reports whether a server is up.
func (c *Client) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.process != nil
}

// LastCrash reports the last crash, if any.
func (c *Client) LastCrash() *Crash { return c.crash.Load() }

// LastReadError reports why a reader ended, which is the difference between a
// server that exited and a stream that was torn down underneath it.
func (c *Client) LastReadError() string {
	if recorded := c.lastReadError.Load(); recorded != nil {
		return *recorded
	}
	return ""
}

// AddHandler registers a handler for the requests the server sends.
func (c *Client) AddHandler(handler Handler) {
	if handler == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers = append(c.handlers, handler)
}

// Ensure starts the server if it is not running, and completes the handshake.
//
// The handshake is a request the server must answer before anything else is sent,
// so it is the one request that does not go through the ordinary path.
func (c *Client) Ensure(ctx context.Context) error {
	c.mu.Lock()
	if c.process != nil {
		c.mu.Unlock()
		return nil
	}
	if c.stopped {
		c.mu.Unlock()
		return fmt.Errorf("lsp: the client was shut down")
	}
	if c.starting {
		c.mu.Unlock()
		return ErrRestarting
	}
	// A server that died is brought back within its bound. Without the bound a
	// server that crashes on every request would restart forever, which is worse
	// than being down.
	crashed := c.crash.Load()
	backoff := time.Duration(0)
	if crashed != nil {
		if c.restarts >= c.config.MaxRestarts {
			c.mu.Unlock()
			return fmt.Errorf("lsp: the server has crashed %d times and is not being restarted again: %w",
				c.restarts, crashed)
		}
		c.restarts++
		// The first restart is immediate, because a crash during a request is usually
		// transient; a server that keeps dying is slowed down on purpose.
		if c.restarts > 1 {
			backoff = c.config.RestartBackoff
		}
	}
	c.starting = true
	c.mu.Unlock()

	if backoff > 0 {
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.mu.Lock()
			c.starting = false
			c.mu.Unlock()
			return ctx.Err()
		}
	}
	err := c.start(ctx)

	c.mu.Lock()
	c.starting = false
	c.mu.Unlock()
	if err != nil {
		return err
	}
	// The crash record is kept rather than cleared. A caller that watches the client
	// recover still wants to know the server died and why, and the restart count is
	// what bounds the loop, not the record.
	return nil
}

func (c *Client) start(ctx context.Context) error {
	starter := c.config.Starter
	if starter == nil {
		starter = sandbox.NewUnrestricted()
	}
	startCtx, cancel := context.WithTimeout(ctx, c.config.StartTimeout)
	defer cancel()
	process, err := starter.Start(startCtx, c.config.Command, c.config.Environment, c.config.Workspace)
	if err != nil {
		return fmt.Errorf("lsp: start %q: %w", c.config.Command[0], err)
	}
	reader := NewReader(process.Stdout())
	writer := NewWriter(process.Stdin())
	trace := c.config.Trace

	c.mu.Lock()
	c.process = process
	c.reader = reader
	c.writer = writer
	c.initialized = false
	c.pending = map[string]*pending{}
	c.openDocuments = map[string]bool{}
	c.mu.Unlock()

	running := make(chan struct{})
	c.mu.Lock()
	c.generation++
	generation := c.generation
	c.running = running
	c.readerGeneration = generation
	c.reader = reader
	c.mu.Unlock()
	go c.readLoop(reader, running, trace, generation)

	stderrDrained := make(chan struct{})
	c.mu.Lock()
	c.stderrDrained = stderrDrained
	c.mu.Unlock()
	go c.drainStderr(process.Stderr(), stderrDrained)

	// The handshake tells the server where the workspace is and what the client can
	// do, and the server's answer is what a later request is checked against.
	initialize, err := json.Marshal(InitializeParams{
		ProcessID: osGetpid(),
		RootURI:   PathToURI(c.config.Workspace),
		RootPath:  c.config.Workspace,
		Capabilities: ClientCapabilities{
			TextDocument: TextDocumentClientCapabilities{
				Synchronization:    PushCapability,
				PublishDiagnostics: PublishDiagnosticsClientCapabilities{RelatedInformation: true},
				Hover:              PushCapability,
				Definition:         PushCapability,
				References:         PushCapability,
				Rename:             PushCapability,
			},
			Workspace: WorkspaceClientCapabilities{WorkspaceFolders: true},
		},
		// The trace is off because the client records what it needs itself, and a
		// server that also traces would double every message for no benefit.
		Trace: "off",
	})
	if err != nil {
		_ = c.abandonStart()
		return fmt.Errorf("lsp: build the initialize request: %w", err)
	}
	// The handshake runs under the start budget rather than the caller's, because
	// the start budget is the one that says what starting a server may cost. A cold
	// language server is launched, may index a workspace before it answers, and is
	// given a request timeout that is meant for a question on a server that is
	// already up. Handing the handshake the caller's context left it bounded by
	// neither, so a start that was merely slow looked like a server that was gone.
	response, err := c.exchangeRaw(startCtx, "initialize", initialize, false)
	if err != nil {
		_ = c.abandonStart()
		return fmt.Errorf("lsp: initialize: %w", err)
	}
	var capabilities ServerCapabilities
	if len(response.Result) > 0 {
		if err := json.Unmarshal(response.Result, &capabilities); err != nil {
			_ = c.abandonStart()
			return fmt.Errorf("lsp: decode the server capabilities: %w", err)
		}
	}
	if err := c.notify("initialized", []byte(`{}`)); err != nil {
		_ = c.abandonStart()
		return fmt.Errorf("lsp: the initialized notification: %w", err)
	}
	c.mu.Lock()
	c.capabilities = capabilities
	c.initialized = true
	c.mu.Unlock()
	return nil
}

// abandonStart tears down a server whose handshake failed.
//
// A server that cannot complete initialize is not usable, and leaving it running
// would hold a process and a set of open documents for a client that is about to
// report the failure to its caller.
func (c *Client) abandonStart() error {
	c.mu.Lock()
	process := c.process
	writer := c.writer
	c.process = nil
	c.writer = nil
	c.reader = nil
	c.initialized = false
	c.mu.Unlock()
	if process == nil {
		return nil
	}
	if writer != nil {
		// The stream is closed before the process so the server sees the end of its
		// input and can exit on its own rather than being killed.
		_ = process.Stdin().Close()
	}
	return process.Close()
}

// readLoop drains the server's stream until it ends.
//
// The generation is checked before anything is torn down. A server that died is
// replaced quickly, and its reader can notice the death after the replacement is
// already up; without the check that reader would clear the new connection's
// pending requests and leave them unanswered forever.
func (c *Client) readLoop(
	reader *Reader, running chan struct{}, trace func(Direction, Message), generation int64,
) {
	defer close(running)
	for {
		message, err := reader.Read()
		if err != nil {
			// The staleness check and the teardown happen under one lock. Checking and
			// then acting separately leaves a window in which a replacement server can
			// start between the two, and the teardown would then clear the live
			// connection's pending requests and leave them unanswered forever.
			// The crash is recorded either way. It is a fact about a server that ran,
			// and a caller that watched the client recover is exactly the one who most
			// wants to know it had happened. Only the teardown is conditional.
			if !errors.Is(err, io.EOF) {
				c.recordCrash(err)
			} else {
				c.recordCrash(fmt.Errorf("the language server closed its stream"))
			}
			waiters, current := c.teardownGeneration(generation)
			if !current {
				// This reader belongs to a server that has already been replaced, so the
				// state it would clear belongs to the live one.
				return
			}
			for _, waiter := range waiters {
				waiter.finish()
			}
			return
		}
		if trace != nil {
			trace(DirectionInbound, message)
		}
		c.dispatch(message)
	}
}

// drainStderr reads the error stream so the server cannot block on a full pipe.
func (c *Client) drainStderr(stream io.ReadCloser, done chan struct{}) {
	defer close(done)
	if stream == nil {
		return
	}
	scanner := bufio.NewScanner(stream)
	// A server is allowed to be verbose on its error stream; the lines are read to
	// keep the pipe moving and not kept, because the protocol stream is the record
	// and this is not.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
	}
	_ = stream.Close()
}

// dispatch routes one inbound message.
func (c *Client) dispatch(message Message) {
	switch {
	case message.IsResponse():
		c.deliverResponse(message)
	case message.IsRequest():
		c.answerRequest(message)
	case message.IsNotification():
		c.handleNotification(message)
	}
}

func (c *Client) deliverResponse(message Message) {
	key := message.ID.String()
	c.mu.Lock()
	waiter, waiting := c.pending[key]
	if waiting {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if !waiting {
		// A response nobody is waiting for is either a duplicate or the answer to a
		// request that already timed out. Either way there is nothing to do, and
		// blocking on it would wedge the reader.
		return
	}
	select {
	case waiter.response <- message:
	default:
	}
	waiter.finish()
}

func (c *Client) answerRequest(message Message) {
	c.mu.Lock()
	handlers := make([]Handler, len(c.handlers))
	copy(handlers, c.handlers)
	trace := c.config.Trace
	c.mu.Unlock()

	ctx := &messageContext{message: message}
	// A server request has to be answered or it waits forever, so a handler that
	// panics still produces a response.
	reply, send := func() (reply Message, send bool) {
		defer func() {
			if recovered := recover(); recovered != nil {
				reply = Message{
					ID: message.ID,
					Error: &ResponseError{
						Code:    codeInternalError,
						Message: fmt.Sprintf("the client handler panicked: %v", recovered),
					},
				}
				send = true
			}
		}()
		for _, handler := range handlers {
			if candidate, ok := handler.Handle(ctx, message); ok {
				candidate.ID = message.ID
				return candidate, true
			}
		}
		return Message{}, false
	}()
	if send {
		if trace != nil {
			trace(DirectionOutbound, reply)
		}
		_ = c.send(reply)
		return
	}
	// An unimplemented request is answered with the protocol's own code rather than
	// dropped, so a server that asks for something optional is not left waiting.
	_ = c.send(Message{
		ID: message.ID,
		Error: &ResponseError{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("the client does not implement %q", message.Method),
		},
	})
}

func (c *Client) handleNotification(message Message) {
	if message.Method == MethodPublishDiagnostics {
		c.diagnostics.Publish(message.Params)
	}
}

// messageContext is the per message state a handler reads.
type messageContext struct {
	message Message
}

// Direction reports which side sent the message.
func (c *messageContext) Direction() Direction { return DirectionInbound }

// Request returns the request, when the message is one.
func (c *messageContext) Request() (Request, bool) {
	if !c.message.IsRequest() {
		return Request{}, false
	}
	return Request{ID: c.message.ID, Method: c.message.Method, Params: c.message.Params}, true
}

// Notification returns the notification, when the message is one.
func (c *messageContext) Notification() (Notification, bool) {
	if !c.message.IsNotification() {
		return Notification{}, false
	}
	return Notification{Method: c.message.Method, Params: c.message.Params}, true
}

// Response returns the response, when the message is one.
func (c *messageContext) Response() (Message, bool) {
	if !c.message.IsResponse() {
		return Message{}, false
	}
	return c.message, true
}

// recordCrash notes that the server went away.
func (c *Client) recordCrash(cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A deliberate shutdown is not a crash, so a reader that ends because the client
	// stopped does not leave a crash record for a caller to chase.
	if c.stopped {
		return
	}
	c.crash.Store(&Crash{At: time.Now().UTC(), Err: cause, Restarts: c.restarts})
}

// teardownGeneration gives up on the in flight requests of the launch a reader
// belongs to, and reports whether that reader is the current one.
//
// The answer and the clearing happen under a single lock, so a reader that has
// already been replaced can never clear the state of the connection that replaced
// it.
func (c *Client) teardownGeneration(generation int64) (waiters map[string]*pending, current bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return nil, false
	}
	waiters = c.pending
	c.pending = map[string]*pending{}
	c.initialized = false
	c.process = nil
	return waiters, true
}

// exchange sends a request and waits for its answer, starting the server if needed.
func (c *Client) exchange(ctx context.Context, method string, params []byte) (Message, error) {
	return c.exchangeRaw(ctx, method, params, true)
}

// exchangeRaw is the one path onto the wire.
//
// The handshake has to go through it without the initialized check, because that
// is the request which sets the flag: routing it through the checked path would
// mean the client can never finish starting.
func (c *Client) exchangeRaw(
	ctx context.Context, method string, params []byte, requireInitialized bool,
) (Message, error) {
	if err := c.Ensure(ctx); err != nil {
		return Message{}, err
	}
	c.mu.Lock()
	if requireInitialized && !c.initialized {
		c.mu.Unlock()
		return Message{}, ErrNotRunning
	}
	writer := c.writer
	c.nextID++
	id := ID{Number: c.nextID}
	key := id.String()
	waiter := &pending{response: make(chan Message, 1), done: make(chan struct{})}
	c.pending[key] = waiter
	c.mu.Unlock()

	outgoing := Message{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if c.config.Trace != nil {
		c.config.Trace(DirectionOutbound, outgoing)
	}
	if err := writer.Write(outgoing); err != nil {
		c.forget(key)
		return Message{}, fmt.Errorf("lsp: send %q: %w", method, err)
	}

	timeout := time.NewTimer(c.config.RequestTimeout)
	defer timeout.Stop()
	select {
	case response := <-waiter.response:
		if response.Error != nil {
			return response, response.Error
		}
		return response, nil
	case <-waiter.done:
		// The connection went away while the request was in flight.
		return c.retryAfterCrash(ctx, method, params, requireInitialized, errServerWentAway)
	case <-ctx.Done():
		c.forget(key)
		return Message{}, ctx.Err()
	case <-timeout.C:
		c.forget(key)
		// A server that is up and slow is not a server that went away, and saying so
		// sends a reader looking for a process that is still running. The handshake
		// is where the two are most easily confused, because a cold server that is
		// indexing before it answers looks exactly like one that died.
		return c.retryAfterCrash(ctx, method, params, requireInitialized, errNoAnswerInTime)
	}
}

// whyARetryStopped says why a question was not asked a second time.
type retryOutcome int

const (
	// errServerWentAway is the connection ending under a question in flight.
	errServerWentAway retryOutcome = iota
	// errNoAnswerInTime is the wait running out with the server still there.
	errNoAnswerInTime
)

// retryAfterCrash brings a dead server back and sends the question once more.
//
// The window between the server dying and the reader noticing it is real, and a
// question sent into that window would otherwise time out even though the answer
// is available from a server that starts again immediately. The retry happens at
// most once per question, and only for a question: a notification is never
// repeated, because repeating one could open a document twice.
func (c *Client) retryAfterCrash(
	ctx context.Context, method string, params []byte, requireInitialized bool,
	outcome retryOutcome,
) (Message, error) {
	if method == MethodInitialize {
		// The handshake has its own path through start, so retrying it here would
		// start a second server. That is the right call and a poor thing to report,
		// because both a server that went away and one that was merely slow arrive
		// here, and telling a caller the server is not running when it is running
		// and slow sends them to look for a process that never died.
		if outcome == errNoAnswerInTime {
			return Message{}, fmt.Errorf(
				"lsp: the language server did not finish the handshake within %v: %w",
				c.config.RequestTimeout, ErrNotRunning)
		}
		return Message{}, ErrNotRunning
	}
	c.mu.Lock()
	alreadyRetried := c.retriedMethods == nil
	if !alreadyRetried {
		if _, seen := c.retriedMethods[method]; seen {
			c.mu.Unlock()
			return Message{}, ErrNotRunning
		}
	}
	if c.retriedMethods == nil {
		c.retriedMethods = map[string]bool{}
	}
	c.retriedMethods[method] = true
	stopped := c.stopped
	c.mu.Unlock()
	if stopped {
		return Message{}, ErrNotRunning
	}
	if err := c.Ensure(ctx); err != nil {
		return Message{}, err
	}
	response, err := c.exchangeRaw(ctx, method, params, requireInitialized)
	c.mu.Lock()
	delete(c.retriedMethods, method)
	c.mu.Unlock()
	return response, err
}

func (c *Client) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if waiter, waiting := c.pending[key]; waiting {
		delete(c.pending, key)
		waiter.finish()
	}
}

// notify sends a notification, which expects no answer.
func (c *Client) notify(method string, params []byte) error {
	c.mu.Lock()
	writer := c.writer
	c.mu.Unlock()
	if writer == nil {
		return ErrNotRunning
	}
	outgoing := Message{JSONRPC: "2.0", Method: method, Params: params}
	if c.config.Trace != nil {
		c.config.Trace(DirectionOutbound, outgoing)
	}
	return writer.Write(outgoing)
}

func (c *Client) send(message Message) error {
	c.mu.Lock()
	writer := c.writer
	c.mu.Unlock()
	if writer == nil {
		return ErrNotRunning
	}
	return writer.Write(message)
}

// Request sends one request to the server, starting it if needed.
func (c *Client) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	encoded, err := encodeParams(params)
	if err != nil {
		return nil, err
	}
	response, err := c.exchange(ctx, method, encoded)
	if err != nil {
		return nil, err
	}
	return response.Result, nil
}

func encodeParams(params any) ([]byte, error) {
	if params == nil {
		return []byte("{}"), nil
	}
	if raw, isRaw := params.(json.RawMessage); isRaw {
		return raw, nil
	}
	if raw, isBytes := params.([]byte); isBytes {
		return raw, nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("lsp: encode the %s parameters: %w", "request", err)
	}
	return encoded, nil
}

// Restart stops the server and starts it again, which is what a caller does after
// changing something the server caches.
func (c *Client) Restart(ctx context.Context) error {
	if err := c.Stop(); err != nil {
		return err
	}
	c.mu.Lock()
	// A deliberate restart spends a restart, because the bound exists to stop a
	// crash loop and a caller looping on Restart is the same problem.
	if c.restarts >= c.config.MaxRestarts {
		c.mu.Unlock()
		return fmt.Errorf("lsp: the client has used its %d restarts", c.config.MaxRestarts)
	}
	c.restarts++
	c.stopped = false
	c.mu.Unlock()
	return c.Ensure(ctx)
}

// Close shuts the server down. It is the same as Stop, and exists so a client can
// be handed to anything that expects an io.Closer.
func (c *Client) Close() error { return c.Stop() }

// Stop shuts the server down.
func (c *Client) Stop() error {
	c.mu.Lock()
	if c.stopped && c.process == nil {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	process := c.process
	running := c.running
	c.process = nil
	c.initialized = false
	c.mu.Unlock()

	if process == nil {
		return nil
	}
	// A graceful shutdown is asked for first, because a server that exits on request
	// saves its state and a killed one does not.
	_ = process.Close()
	if running != nil {
		select {
		case <-running:
		case <-time.After(2 * time.Second):
			// The read loop is still draining, and the process is already being
			// closed, so waiting longer would only delay the caller.
		}
	}
	return nil
}

// Wait blocks until the server's stream ends, which a test and a shutdown path
// both want.
func (c *Client) Wait() {
	c.mu.Lock()
	running := c.running
	c.mu.Unlock()
	if running == nil {
		return
	}
	<-running
}

// osGetpid is a variable so a test can pin the identifier the server is told.
var osGetpid = func() int { return 1 }

// codeInternalError is the protocol's own code for a client side failure, used
// when a handler panics so a server sees a proper error rather than a hang.
const codeInternalError = -32603
