// SPDX-License-Identifier: Apache-2.0

// Package web is the main way into MarxAgent: a local HTTP server, bound to the
// loopback interface, that serves the interface and the event stream behind it.
//
// It is assembly rather than a part. Deciding that the main form of interaction is a
// browser on this machine, and that it needs a token and a port, is a decision. How
// the token is generated, and what a frame on the wire looks like, are not.
//
// Three things are fixed by the design and are not this package's to choose:
//
//   - It binds 127.0.0.1 and nothing else. A server that listened on every interface
//     would put an unauthenticated agent on the network, and the token is only a
//     speed bump against other processes on the same machine, not a defence against
//     the internet.
//   - It does not use a Unix domain socket, even where the host has them, because
//     older Windows does not and one code path that works everywhere beats two that
//     are almost the same.
//   - It reaches nothing outside this machine. The interface is embedded in the
//     binary, so there is no CDN to fetch from and no telemetry to send, and the
//     server makes no outbound request of its own.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Errors this package reports.
var (
	// ErrNoToken is a server built without a token. It is refused rather than
	// defaulted, because a server that made one up and logged it where nobody looks
	// is a server whose protection is a coin toss.
	ErrNoToken = errors.New("web: the server needs a token")
	// ErrNoListener is a server with nowhere to serve from.
	ErrNoListener = errors.New("web: the server needs a listener")
	// ErrNoStream is a server with nothing to serve the event stream from.
	ErrNoStream = errors.New("web: the server needs an event stream")
)

// Config is how a server is built.
//
// The zero value is not usable and is not meant to be. A field left empty here is
// either a security setting or the place the server listens, and neither is worth
// guessing at: a default address would be a decision about the machine's trust
// boundary, and a default token would be no token at all.
type Config struct {
	// Listener is where to serve. It is required, and it is a listener rather than an
	// address so that the caller decides the port and can report it once it is known.
	Listener net.Listener
	// Token authorises a request. It is required.
	Token string
	// WebDir serves the interface from this directory instead of from the copy in the
	// binary. It is for development, where a change to the interface should be
	// visible without the program being rebuilt first.
	//
	// It is not a deployment option. The shipping promise is one file with nothing
	// beside it, and a directory of files that has to travel with the program is the
	// thing that promise is instead of.
	WebDir string
	// Stream is where the event stream comes from. It is required, because a server
	// that accepted a connection and had nothing to send would leave a window
	// watching an empty panel and calling it a quiet conversation.
	Stream Stream
	// Handler serves everything that is not the interface itself, which is how the
	// caller puts its own routes alongside. Optional.
	Handler http.Handler
	// Debug turns on the router's own development behaviour: the route table printed
	// on every start, and a banner on every response. It is off by default because a
	// mode that is only switched on somewhere is a mode nobody has looked at there.
	Debug bool
	// ReadHeaderTimeout bounds how long a client may take to send its headers.
	// Optional, and defaulted, because a server that waits forever for a header is a
	// server that can be made to hold a connection open for nothing.
	ReadHeaderTimeout time.Duration
}

// defaultReadHeaderTimeout is long enough for a slow link and short enough that a
// connection which sends nothing costs almost nothing.
const defaultReadHeaderTimeout = 10 * time.Second

// NewToken returns a token for this run.
//
// It is 32 bytes of the system randomness, which is far more than a guess can work
// through, and it is compared in constant time wherever it is checked. A token that
// was short, or derived from something predictable, would make the whole scheme
// decorative.
//
// The caller is given the token rather than having it logged, because a token in a
// log file is a token in every backup of that file.
func NewToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("web: generate a token: %w", err)
	}
	// Raw bytes in a URL are awkward and need escaping, so they are encoded. This is
	// not a password and needs no hashing: it is compared, never derived from.
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Listen listens on an address, refusing anything that is not on the loopback
// interface.
//
// The refusal is the point of the function. A caller passing "0.0.0.0:8787" has asked
// for something this project will not do: an agent with a token as the only thing
// between it and everything on the network, on a machine whose owner never intended
// that. Saying so is better than binding it, because a listener that came up is a
// listener somebody will have to remember to shut down.
//
// A host with no port is given one, because that is the ordinary case and the host
// is the right party to choose it.
func Listen(addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		// A bare port is taken as a request for one on the loopback interface, which is
		// what somebody writing ":8787" means.
		if strings.HasPrefix(addr, ":") {
			host, port = "127.0.0.1", strings.TrimPrefix(addr, ":")
		} else {
			return nil, fmt.Errorf("web: %q is not an address to listen on: %w", addr, err)
		}
	}
	if port == "" || port == "0" {
		port = "0"
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
	default:
		return nil, fmt.Errorf(
			"web: refusing to listen on %q; this server serves the loopback interface only", addr)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, fmt.Errorf("web: listen on %s: %w", addr, err)
	}
	return listener, nil
}

// LoopbackListener listens on the loopback interface, on a port the host chooses.
//
// A port of zero is the point: the caller finds out which port it got and can put
// that in a link, rather than picking a number and colliding with something else on
// the machine. The interface is loopback on purpose, for the reason in the package
// comment, and it is stated here rather than left to a default.
func LoopbackListener() (net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("web: listen on the loopback interface: %w", err)
	}
	return listener, nil
}

// Server serves the interface and the stream behind it.
type Server struct {
	config   Config
	listener net.Listener
	http     *http.Server
	// handler is the whole server, the interface and the routes together.
	handler http.Handler
	// mounted is what the server answers, so a caller can be told rather than having
	// to find out with a 404.
	mounted mounted
	// events owns the open streams, so a caller can see how many windows are watching.
	events *sseHandler
	// closing is closed when a stop begins, so the open streams can finish rather than
	// being waited for. Nothing waits for a stream on its own; a stream can be open
	// for hours, and a stop cannot take hours.
	closing chan struct{}
	// closeOnce guards the closing channel, because Start can be reached again and a
	// second close of a closed channel is a panic.
	closeOnce sync.Once
}

// Watchers reports how many windows currently have a stream open.
//
// It is here so the multi-window case can be checked rather than claimed. A server
// that says two windows are watching and then only ever tells one of them is not
// synchronised, whatever it says here.
func (s *Server) Watchers() int { return s.events.Windows() }

// Mounted reports the addresses the server answers on.
//
// It is here so the command that starts it can print them, rather than the addresses
// being written down twice in two places that then disagree.
func (s *Server) Mounted() mounted { return s.mounted }

// Resumes reports how many connections have carried on from a position rather than
// starting again.
//
// It is the evidence that resuming works. A count that stayed at zero would mean the
// mechanism is untested, and a mechanism for staying connected that is never used is
// a mechanism that will not work when it is needed.
func (s *Server) Resumes() int { return s.events.Resumers() }

// New builds a server.
//
// It does not start anything. A caller that has a server in hand has not yet
// committed a port, and the token can be handed out before anything is listening, so
// there is no window where a listener exists that nobody has the token for.
func New(config Config) (*Server, error) {
	if config.Listener == nil {
		return nil, ErrNoListener
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, ErrNoToken
	}
	if config.Stream == nil {
		return nil, ErrNoStream
	}
	if config.ReadHeaderTimeout <= 0 {
		config.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	server := &Server{config: config, listener: config.Listener}

	// The channel the open streams watch so they can end on a stop rather than being
	// waited for. It is made before the routes are attached, because a stream handler
	// is handed it and a nil channel is a handler that blocks for ever.
	server.closing = make(chan struct{})
	server.events = newSSEHandler(config.Stream, server.closing)

	router := newRouter(config.Debug)
	server.mounted = server.routes(router)
	server.handler = router

	server.http = &http.Server{
		Handler:           server.handler,
		ReadHeaderTimeout: config.ReadHeaderTimeout,
		// The write timeout is left unset on purpose. A stream of events can be idle
		// for a long time while a model thinks, and a write deadline would cut a
		// perfectly healthy connection off mid-turn. The read header timeout above is
		// what bounds a client that has sent nothing at all.
	}
	return server, nil
}

// Addr is where the server is listening.
//
// It is asked for after the server is built and before it is started, so a caller can
// put a real port in a link instead of the one it hoped for.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Token is the token this server expects.
//
// It is exposed so the caller can hand it to whoever needs it, which is the whole
// reason it was not logged.
func (s *Server) Token() string { return s.config.Token }

// URL is the address the interface is served at, with the token already in it.
//
// The token goes in the fragment rather than the query because a fragment is never
// sent to the server and never lands in an access log. A token in a query string is
// a token in every log between here and the interface.
func (s *Server) URL() string {
	return fmt.Sprintf("http://%s/#token=%s", s.Addr(), s.config.Token)
}

// matchesToken compares in constant time, and in a way that does not leak how much of
// what was presented was right.
//
// A token that can be discovered by timing the difference between a near miss and a
// miss is a token that can be discovered. The comparison is over the whole presented
// value rather than up to its length, so the time taken says only whether the answer
// was right.
func matchesToken(presented, expected string) bool {
	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// tokenCookie is where the interface keeps the token once it has it.
//
// A cookie rather than local storage because a cookie goes out with the request, so
// the interface needs no code to attach it, and because it is scoped to the host and
// path rather than being readable by any other page served from anywhere.
const tokenCookie = "marxagent_token"

// Start serves until the context is done.
//
// The listener is closed on the way out even if the shutdown below fails, because a
// server that has been told to stop and is still holding its port is a server that
// will not start again.
func (s *Server) Start(ctx context.Context) error {
	done := make(chan struct{})
	// A goroutine per Start, closed on the way out. It exits with the server rather
	// than outliving it, because a goroutine watching a context nobody will cancel
	// again is a goroutine that has leaked with nobody left to notice.
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
	}()

	errs := make(chan error, 1)
	go func() {
		errs <- s.http.Serve(s.listener)
	}()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("web: serve: %w", err)
	case <-ctx.Done():
		// The open streams are told first, so the shutdown below is not waiting on
		// connections that are never going to finish on their own.
		s.closeOnce.Do(func() { close(s.closing) })
		// The shutdown gets its own budget rather than being given the cancelled
		// context, because a context that is already done would cut the shutdown off
		// at the first step and leave connections open.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		shutdownErr := s.http.Shutdown(shutdownCtx)
		closeErr := s.listener.Close()
		<-errs
		if shutdownErr != nil {
			return fmt.Errorf("web: shut down: %w", shutdownErr)
		}
		if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			return fmt.Errorf("web: close the listener: %w", closeErr)
		}
		return nil
	}
}

// shutdownBudget is how long a stop may take before connections are cut instead.
//
// It matches the durability budget the rest of the project uses, so that a shutdown
// takes about as long whichever part is asked to stop, rather than one part being
// generous and another impatient.
const shutdownBudget = 5 * time.Second
