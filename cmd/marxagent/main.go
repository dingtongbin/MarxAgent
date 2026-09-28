// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/assembly/api"
	"github.com/dingtongbin/MarxAgent/internal/assembly/config"
	"github.com/dingtongbin/MarxAgent/internal/assembly/mode"
	"github.com/dingtongbin/MarxAgent/internal/assembly/runtime"
	"github.com/dingtongbin/MarxAgent/internal/assembly/ui/web"
	"github.com/dingtongbin/MarxAgent/internal/assembly/workspace"
	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

func main() {
	if err := run(); err != nil {
		// Being stopped is not a failure, so it exits quietly. A line of text on
		// somebody's way out is a line of text they did not ask for, and a script
		// cannot tell "try again" from "this will never work" if both exit 1.
		if errors.Is(err, context.Canceled) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "marxagent:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		serve   = flag.Bool("serve", false, "serve the interface on the loopback interface")
		onlyURL = flag.Bool("url", false, "print the address with the token, then stop")
		webDir  = flag.String("web-dir", "",
			"serve the interface from this directory instead of the binary; for development")
		addr = flag.String("addr", defaultAddr(),
			"the loopback address to serve on; port 0 lets the host choose")
	)
	debug := flag.Bool("debug", envBool("MARXAGENT_DEBUG"),
		"print the route table and answer in the router's development manner")
	flag.Parse()

	if !*serve && !*onlyURL {
		// Doing nothing is worth saying rather than sitting there looking broken.
		fmt.Fprintln(os.Stderr, "marxagent: nothing to do. Try -serve, or -url to print the address.")
		return nil
	}

	// The signal context is how a stop is arranged, on every platform the project
	// supports, rather than by handling each platform's notion of interruption. The
	// platform differences belong in the platform package, not spread through the
	// entry point.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	server, release, err := startServer(*webDir, *addr, *debug)
	if err != nil {
		return err
	}
	if *webDir != "" {
		// Said out loud, because a developer who has asked for this and then sees a
		// stale interface has usually forgotten they asked. Saying it once here is
		// cheaper than debugging it.
		fmt.Printf("Serving the interface from %s (not from the binary).\n", *webDir)
	}
	if *onlyURL {
		// The address has a port the host chose and a token that is different every
		// run, so somebody scripting this needs it without reading it off a screen.
		fmt.Println(server.URL())
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownBudget)
		defer cancel()
		return serveAndRelease(server, release, shutdown)
	}

	fmt.Printf("MarxAgent is serving on %s\n", server.URL())
	fmt.Println("Press Ctrl+C to stop.")
	return serveAndRelease(server, release, ctx)
}

// shutdownBudget is how long a stop may take in total.
//
// It is the budget the design gives the whole exit sequence, and it is spent on
// the server and on the conversations together rather than on each of them, so a
// stop takes about as long whichever part is asked to stop.
const shutdownBudget = 5 * time.Second

// serveAndRelease serves until the context is done, then releases what the
// program holds.
//
// The release runs on a fresh context rather than the cancelled one. A context
// that is already done would cut the flush off at its first step and leave
// whatever had not been written unwritten, which is the opposite of what stopping
// is for.
func serveAndRelease(server *web.Server, release func(context.Context) error, ctx context.Context) error {
	serveErr := server.Start(ctx)
	if ctx.Err() == nil {
		// It ended on its own, which is not a stop and not the point at which a
		// shutdown sequence belongs.
		_ = release(context.Background())
		return serveErr
	}
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()
	if err := release(shutdown); err != nil {
		// Said rather than swallowed: a stop that could not write everything down
		// is the one case where somebody needs to know before they close the
		// window and expect to find the conversation there next time.
		fmt.Fprintln(os.Stderr, "marxagent: shutdown:", err)
		if serveErr == nil {
			return err
		}
	}
	return serveErr
}

// defaultAddr is where to serve when nobody said.
//
// The environment is consulted first because the development runner sets a fixed port
// there, and the Vite dev server has to be able to forward the event stream to a known
// address. A port of zero is the right default otherwise: the host chooses, and
// nothing collides with anything else on the machine.
func defaultAddr() string {
	if fromEnv := strings.TrimSpace(os.Getenv("MARXAGENT_ADDR")); fromEnv != "" {
		return fromEnv
	}
	return "127.0.0.1:0"
}

// envBool reads a flag out of the environment, for a development runner that cannot
// pass arguments.
func envBool(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}

// startServer assembles the program and serves it.
//
// The order is the shutdown order reversed, and it is written in one function so
// that the two cannot drift: a configuration and a storage scope are opened, the
// conversations in them are brought back, the runtime is built over those, the
// interface's own routes are attached to it, and the server is handed both the
// routes and the event stream.
//
// Everything that has to be released is registered for release before the
// listener is opened, so a failure part way through cannot leave a ledger locked
// on a machine the next start would then refuse to open.
func startServer(webDir, addr string, debug bool) (*web.Server, func(context.Context) error, error) {
	// The host description is asked for here, once, before anything that could
	// build a snapshot runs: a conversation's standing context then reads a
	// cache rather than paying a kernel call in the middle of a turn.
	mode.PrimeSystem()
	settings, err := openConfig()
	if err != nil {
		return nil, nil, err
	}
	scope, err := storage.AppDataScope()
	if err != nil {
		return nil, nil, err
	}
	// The tab strips live beside the conversations rather than inside them,
	// because a tab is what a window is looking at and a conversation is what was
	// said: one window can have the same conversation open twice and two windows
	// can have none of each other's conversations open.
	tabs, err := workspace.Open(filepath.Join(scope.DataRoot(), "workspaces"))
	if err != nil {
		_ = scope.Close()
		return nil, nil, err
	}
	sessions, err := runtime.New(runtime.Options{
		Config:       settings,
		AppDataScope: scope,
		Logger:       logAssembly,
	})
	if err != nil {
		_ = scope.Close()
		return nil, nil, err
	}

	token, err := web.NewToken()
	if err != nil {
		_ = closeRuntime(sessions)
		return nil, nil, err
	}
	listener, err := web.Listen(addr)
	if err != nil {
		_ = closeRuntime(sessions)
		return nil, nil, err
	}
	handler := api.New(sessions, settings, tabs, logAssembly)
	server, err := web.New(web.Config{
		Listener: listener,
		Token:    token,
		// The runtime rather than its event hub, so that a window asking to watch
		// a conversation this program has never heard of is refused instead of
		// being answered with a stream that will never speak.
		Stream:  sessions,
		Handler: handler,
		WebDir:  webDir,
		Debug:   debug,
	})
	if err != nil {
		// The listener is closed on the way out, because a caller that got an error
		// will not know it was holding a port.
		_ = listener.Close()
		_ = closeRuntime(sessions)
		return nil, nil, err
	}
	return server, closeRuntime(sessions), nil
}

// closeRuntime is the shutdown sequence for everything the runtime owns.
//
// The budget belongs to the caller, so this function takes the context rather
// than making one: a shutdown that took its own time would be a shutdown whose
// duration nothing could bound. A conversation still running is cancelled before
// the flush, because a turn that is still producing records would otherwise be
// racing the flush that is meant to be the last thing that happens to it.
func closeRuntime(sessions *runtime.Runtime) func(context.Context) error {
	return func(ctx context.Context) error {
		return sessions.Close(ctx)
	}
}

// openConfig loads the configuration for the directory the program was started
// in, which is where a project configuration file would be.
func openConfig() (*config.Store, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("marxagent: locate the working directory: %w", err)
	}
	return config.Open(root)
}

// logAssembly reports what the assembly layer did, through the standard logger.
//
// It is slog rather than a print because the project freezes its log fields and
// a second format on the same machine makes them worthless. A window's problem
// is a fact about the machine, and this is where it is written down.
func logAssembly(message string, fields map[string]any) {
	if len(fields) == 0 {
		slog.Info(message)
		return
	}
	slog.Info(message, fieldsToAttributes(fields)...)
}

func fieldsToAttributes(fields map[string]any) []any {
	attributes := make([]any, 0, len(fields))
	for key, value := range fields {
		attributes = append(attributes, key, value)
	}
	return attributes
}
