// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolNilReceiverIsSafe(t *testing.T) {
	var tool *Tool
	if tool.Name() != "" || tool.Description() != "" {
		t.Fatal("nil tool returned metadata")
	}
	if string(tool.Parameters()) != `{"type":"object","properties":{}}` {
		t.Fatalf("nil parameters = %s", tool.Parameters())
	}
	if _, err := tool.Execute(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil tool execute = %v", err)
	}
	detached := &Tool{serverName: "srv", remoteName: "remote"}
	if strings.Contains(detached.Description(), "MCP tool") == false {
		t.Fatalf("fallback description = %q", detached.Description())
	}
	if !strings.Contains(detached.Description(), `"remote"`) || !strings.Contains(detached.Description(), `"srv"`) {
		t.Fatalf("fallback description = %q", detached.Description())
	}
	if _, err := (&Tool{}).Execute(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("detached tool execute = %v", err)
	}
}

func TestQualifiedNameValidation(t *testing.T) {
	name, err := QualifiedName("srv", "remote")
	if err != nil || name != "mcp__srv__remote" {
		t.Fatalf("qualified name = %q err = %v", name, err)
	}
	if _, err := QualifiedName("", "remote"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty server name = %v", err)
	}
	if _, err := QualifiedName("srv", ""); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty tool name = %v", err)
	}
	if _, err := QualifiedName(strings.Repeat("s", 129), "remote"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("long server name = %v", err)
	}
	if err := validateName("ok.name-1_2"); err != nil {
		t.Fatal(err)
	}
	if err := validateName("has space"); err == nil {
		t.Fatal("invalid character accepted")
	}
}

func TestClientConfigValidation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		config ClientConfig
	}{
		{name: "empty name", config: ClientConfig{Command: "server"}},
		{name: "empty command", config: ClientConfig{Name: "srv"}},
		{name: "NUL command", config: ClientConfig{Name: "srv", Command: "bad\x00cmd"}},
		{name: "NUL directory", config: ClientConfig{Name: "srv", Command: "server", Dir: "bad\x00dir"}},
		{name: "NUL argument", config: ClientConfig{Name: "srv", Command: "server", Args: []string{"bad\x00arg"}}},
		{name: "NUL environment", config: ClientConfig{Name: "srv", Command: "server", Env: []string{"A=bad\x00value"}}},
		{name: "missing directory", config: ClientConfig{Name: "srv", Command: "server", Dir: filepath.Join(t.TempDir(), "missing")}},
		{name: "file directory", config: ClientConfig{Name: "srv", Command: "server", Dir: file}},
		{name: "negative page limit", config: ClientConfig{Name: "srv", Command: "server", MaxToolPages: -1}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewClient(test.config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestValidateArgumentsRules(t *testing.T) {
	if err := validateArguments(nil); err != nil {
		t.Fatal(err)
	}
	if err := validateArguments(json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := validateArguments(json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if err := validateArguments(json.RawMessage(`[]`)); err == nil {
		t.Fatal("array arguments accepted")
	}
	if err := validateArguments(json.RawMessage(`null`)); err == nil {
		t.Fatal("null arguments accepted")
	}
}

func TestMarshalSchemaRules(t *testing.T) {
	fallback := `{"type":"object","properties":{}}`
	if got, err := marshalSchema(nil); err != nil || string(got) != fallback {
		t.Fatalf("nil schema = %s err = %v", got, err)
	}
	if got, err := marshalSchema(nil); err != nil || string(got) != fallback {
		t.Fatalf("null schema = %s err = %v", got, err)
	}
	if _, err := marshalSchema([]string{"not", "an", "object"}); err == nil {
		t.Fatal("array schema accepted")
	}
	if _, err := marshalSchema(func() {}); err == nil {
		t.Fatal("unmarshalable schema accepted")
	}
	got, err := marshalSchema(map[string]any{"type": "object"})
	if err != nil || !strings.Contains(string(got), `"object"`) {
		t.Fatalf("schema = %s err = %v", got, err)
	}
}

func TestCloneToolsSortsByQualifiedName(t *testing.T) {
	tools := cloneTools(map[string]*Tool{
		"mcp__b__two": {qualified: "mcp__b__two"},
		"mcp__a__one": {qualified: "mcp__a__one"},
	})
	if len(tools) != 2 || tools[0].qualified != "mcp__a__one" {
		t.Fatalf("tools = %#v", tools)
	}
	if len(cloneTools(nil)) != 0 {
		t.Fatal("nil map produced tools")
	}
}

func TestNilClientAccessorsAreSafe(t *testing.T) {
	var client *Client
	if client.Name() != "" || client.IsStarted() {
		t.Fatal("nil client returned state")
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil start = %v", err)
	}
	if err := client.Health(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil health = %v", err)
	}
}

func TestClientRejectsNilAndCanceledContexts(t *testing.T) {
	client, serverSession := newTestClient(t, false)
	defer serverSession.Close()
	if err := client.Start(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil start = %v", err)
	}
	if _, err := client.Tools(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil tools = %v", err)
	}
	if _, err := client.RefreshTools(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil refresh = %v", err)
	}
	if _, err := client.Tool(nil, "mcp__test__echo"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil tool lookup = %v", err)
	}
	if err := client.Health(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil health = %v", err)
	}
	if err := client.Health(context.Background()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("health before start = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start = %v", err)
	}
	if _, err := client.Tools(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled tools = %v", err)
	}
	if _, err := client.RefreshTools(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh = %v", err)
	}
	if err := client.Health(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled health = %v", err)
	}
	if _, err := client.Tool(context.Background(), "mcp__test__absent"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("missing tool = %v", err)
	}
}

func TestHealthInvalidatesDeadSessions(t *testing.T) {
	client, serverSession := newTestClient(t, false)
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := serverSession.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Health(context.Background()); err == nil {
		t.Fatal("health succeeded against a closed server")
	}
	if client.IsStarted() {
		t.Fatal("failed health did not invalidate the session")
	}
	if err := client.Health(context.Background()); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("health after invalidation = %v", err)
	}
}

func TestToolExecuteRejectsMissingServerTool(t *testing.T) {
	client, serverSession := newTestClient(t, false)
	defer serverSession.Close()
	tools, err := client.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Execute(context.Background(), json.RawMessage(`{"name":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Execute(context.Background(), json.RawMessage(`{"name":"a","extra":1}`)); err != nil {
		t.Fatal(err)
	}
	broken := &Tool{client: client, serverName: "test", remoteName: "absent", qualified: "mcp__test__absent"}
	if _, err := broken.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("calling an unknown server tool succeeded")
	}
}

type failingProcess struct{}

func (failingProcess) Stdin() io.WriteCloser { return nil }
func (failingProcess) Stdout() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (failingProcess) Stderr() io.ReadCloser { return nil }
func (failingProcess) Wait() (sandbox.Result, error) {
	return sandbox.Result{ExitCode: -1}, errors.New("process failed")
}
func (failingProcess) Close() error { return nil }

type failingRunner struct{}

func (failingRunner) Start(context.Context, []string, []string, string) (sandbox.Process, error) {
	return failingProcess{}, nil
}
func (failingRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Backend: "failing", DefaultDeny: true}
}

func TestSandboxTransportRejectsUnusableProcesses(t *testing.T) {
	var transport *sandboxTransport
	if _, err := transport.Connect(context.Background()); !errors.Is(err, sandbox.ErrRequired) {
		t.Fatalf("nil transport = %v", err)
	}
	if _, err := (&sandboxTransport{}).Connect(context.Background()); !errors.Is(err, sandbox.ErrRequired) {
		t.Fatalf("nil runner = %v", err)
	}
	broken := &sandboxTransport{runner: failingRunner{}, config: ClientConfig{Name: "srv", Command: "server"}}
	if _, err := broken.Connect(context.Background()); err == nil {
		t.Fatal("process without stdin was accepted")
	}
}

func TestPoolRejectsNilReceiverAndInvalidConfigs(t *testing.T) {
	var pool *Pool
	if err := pool.Add(ServerConfig{Name: "srv", Command: "server"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil add = %v", err)
	}
	if err := pool.Activate(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil activate = %v", err)
	}
	if err := pool.Activate(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil context activate = %v", err)
	}
	if _, err := pool.Tools(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil tools = %v", err)
	}
	if _, err := pool.Tools(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil context tools = %v", err)
	}
	if _, err := pool.RefreshTools(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil refresh = %v", err)
	}
	if _, err := pool.RefreshTools(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil context refresh = %v", err)
	}
	if err := pool.Health(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil health = %v", err)
	}
	if err := pool.Health(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil context health = %v", err)
	}
	if err := pool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(nil); err != nil {
		t.Fatalf("nil pool close must stay a no-op: %v", err)
	}
	if client, ok := pool.Get("srv"); client != nil || ok {
		t.Fatal("nil pool returned a client")
	}
	if pool.Names() != nil {
		t.Fatal("nil pool returned names")
	}
	if _, err := NewPool([]ServerConfig{{Name: "bad name", Command: "server"}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid pool config = %v", err)
	}
	real, err := NewPool([]ServerConfig{{Name: "srv", Command: "server"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := real.Close(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil context close = %v", err)
	}
	if err := real.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPoolActivatesEveryServerAndRefreshesTools(t *testing.T) {
	first, firstServer := newNamedTestClient(t, "alpha", false)
	defer firstServer.Close()
	second, secondServer := newNamedTestClient(t, "beta", false)
	defer secondServer.Close()
	pool := &Pool{clients: map[string]*Client{"alpha": first, "beta": second}, names: []string{"beta", "alpha"}}
	if err := pool.Activate(context.Background()); err != nil {
		t.Fatal(err)
	}
	refreshed, err := pool.RefreshTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 2 {
		t.Fatalf("refreshed = %#v", refreshed)
	}
	if err := pool.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pool.Health(context.Background(), "missing"); err == nil {
		t.Fatal("health of an unknown server succeeded")
	}
}

func TestPoolDetectsDuplicateQualifiedTools(t *testing.T) {
	first, firstServer := newNamedTestClient(t, "dup", false)
	defer firstServer.Close()
	second, secondServer := newNamedTestClient(t, "dup", false)
	defer secondServer.Close()
	duplicate := &Pool{clients: map[string]*Client{"a": first, "b": second}, names: []string{"a", "b"}}
	if _, err := duplicate.Tools(context.Background()); !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("duplicate tools = %v", err)
	}
	if _, err := duplicate.RefreshTools(context.Background()); !errors.Is(err, ErrDuplicateTool) {
		t.Fatalf("duplicate tools after refresh = %v", err)
	}
}

func TestPoolCloseIsIdempotent(t *testing.T) {
	client, serverSession := newTestClient(t, false)
	defer serverSession.Close()
	pool := &Pool{clients: map[string]*Client{"test": client}, names: []string{"test"}}
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.IsStarted() {
		t.Fatal("pool close did not close the client")
	}
	if err := pool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newPagedTestClient(t *testing.T, maxToolPages int) (*Client, *mcpsdk.ServerSession) {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "paged-server", Version: "1"}, &mcpsdk.ServerOptions{PageSize: 1})
	for _, name := range []string{"alpha", "beta"} {
		toolName := name
		mcpsdk.AddTool(server, &mcpsdk.Tool{
			Name:        toolName,
			Description: "Paged tool " + toolName + ".",
			InputSchema: map[string]any{"type": "object"},
		}, func(ctx context.Context, request *mcpsdk.CallToolRequest, input echoInput) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: toolName}}}, nil, nil
		})
	}
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{Name: "paged", Command: "unused", MaxToolPages: maxToolPages})
	if err != nil {
		serverSession.Close()
		t.Fatal(err)
	}
	client.connectFn = func(ctx context.Context) (*mcpsdk.ClientSession, error) {
		return client.sdk.Connect(ctx, clientTransport, nil)
	}
	return client, serverSession
}

func TestDiscoveryFollowsToolPagination(t *testing.T) {
	client, serverSession := newPagedTestClient(t, 0)
	defer serverSession.Close()
	tools, err := client.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name() != "mcp__paged__alpha" || tools[1].Name() != "mcp__paged__beta" {
		t.Fatalf("paged tools = %#v", tools)
	}
	if _, err := tools[0].Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryEnforcesToolPageLimit(t *testing.T) {
	client, serverSession := newPagedTestClient(t, 1)
	defer serverSession.Close()
	if _, err := client.Tools(context.Background()); err == nil || !strings.Contains(err.Error(), "page limit") {
		t.Fatalf("page limit error = %v", err)
	}
}

func TestClientToolExecuteSurfacesTransportFailures(t *testing.T) {
	client, err := NewClient(ServerConfig{Name: "offline", Command: "missing-server"})
	if err != nil {
		t.Fatal(err)
	}
	client.connectFn = func(context.Context) (*mcpsdk.ClientSession, error) {
		return nil, errors.New("connect refused")
	}
	if _, err := client.Tools(context.Background()); err == nil || !strings.Contains(err.Error(), "connect refused") {
		t.Fatalf("connect failure = %v", err)
	}
	tool := &Tool{client: client, serverName: "offline", remoteName: "echo", qualified: "mcp__offline__echo"}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("execute against a dead client succeeded")
	}
	var _ core.Tool = tool
}
