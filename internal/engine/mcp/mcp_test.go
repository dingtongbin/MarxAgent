// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Name string `json:"name"`
}

func TestClientUsesStdioCommandTransport(t *testing.T) {
	workspace := t.TempDir()
	sandboxEngine, err := sandbox.New(sandbox.Policy{
		Workspace:         workspace,
		WorkspaceWritable: true,
		Network:           true,
		Subprocess:        true,
		EnvAllowlist:      []string{"PATH", "HOME", "USERPROFILE", "SystemRoot", "windir", "ComSpec", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "MARXAGENT_MCP_HELPER"},
		Timeout:           60 * time.Second,
	})
	if err != nil {
		if errors.Is(err, sandbox.ErrUnsupported) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	client, err := NewClient(ServerConfig{
		Name:              "command",
		Command:           os.Args[0],
		Args:              []string{"-test.run=TestMCPHelperProcess", "--"},
		Env:               []string{"MARXAGENT_MCP_HELPER=1"},
		TerminateDuration: time.Second,
		Sandbox:           sandboxEngine,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tools, err := client.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name() != "mcp__command__echo" {
		t.Fatalf("tools = %#v", tools)
	}
	result, err := tools[0].Execute(ctx, json.RawMessage(`{"name":"stdio"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Output), "hello stdio") {
		t.Fatalf("result = %s", result.Output)
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("MARXAGENT_MCP_HELPER") != "1" {
		return
	}
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "helper-server", Version: "1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "echo",
		Description: "Echo a message.",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, request *mcpsdk.CallToolRequest, input echoInput) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "hello " + input.Name}}}, nil, nil
	})
	if err := server.Run(context.Background(), &mcpsdk.StdioTransport{}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestClientDiscoversAndCallsOfficialSDKTool(t *testing.T) {
	client, serverSession := newTestClient(t, false)
	defer serverSession.Close()

	tools, err := client.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools length = %d", len(tools))
	}
	tool := tools[0]
	if tool.Name() != "mcp__test__echo" {
		t.Fatalf("tool name = %q", tool.Name())
	}
	if tool.Description() != "Echo a message." {
		t.Fatalf("description = %q", tool.Description())
	}
	if !strings.Contains(string(tool.Parameters()), `"name"`) {
		t.Fatalf("parameters = %s", tool.Parameters())
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"world"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(string(result.Output), "hello world") {
		t.Fatalf("result = %#v", result)
	}
	if !client.IsStarted() {
		t.Fatal("client is not marked started")
	}
}

func TestClientPreservesMCPErrorResults(t *testing.T) {
	client, serverSession := newTestClient(t, true)
	defer serverSession.Close()

	tools, err := client.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools[0].Execute(context.Background(), json.RawMessage(`{"name":"world"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Error == "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestClientCachesDiscoveryAndRefreshes(t *testing.T) {
	client, serverSession := newTestClient(t, false)
	defer serverSession.Close()
	ctx := context.Background()
	first, err := client.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Tools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] != second[0] {
		t.Fatal("discovery did not use the cached tool")
	}
	refreshed, err := client.RefreshTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 1 || refreshed[0].Name() != "mcp__test__echo" {
		t.Fatalf("refreshed = %#v", refreshed)
	}
}

func TestClientRejectsInvalidArgumentsAndConfiguration(t *testing.T) {
	if _, err := NewClient(ClientConfig{Name: "bad name", Command: "server"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("config error = %v", err)
	}
	if _, err := QualifiedName("server", "bad name"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("qualified name error = %v", err)
	}
	client, serverSession := newTestClient(t, false)
	defer serverSession.Close()
	if _, err := client.Tools(context.Background()); err != nil {
		t.Fatal(err)
	}
	tool, err := client.Tool(context.Background(), "mcp__test__echo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`[]`)); err == nil {
		t.Fatal("array arguments accepted")
	}
	if _, err := tool.Execute(nil, json.RawMessage(`{}`)); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestCommandClientRequiresSandbox(t *testing.T) {
	client, err := NewClient(ServerConfig{Name: "unrestricted", Command: "missing-server"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Tools(context.Background()); !errors.Is(err, sandbox.ErrRequired) {
		t.Fatalf("missing sandbox error = %v", err)
	}
}

func TestClientCloseIsIdempotentAndHonorsCancellation(t *testing.T) {
	client, serverSession := newTestClient(t, false)
	defer serverSession.Close()
	if err := client.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.IsStarted() {
		t.Fatal("closed client is marked started")
	}
	if err := client.Close(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil-context close error = %v", err)
	}
}

func TestPoolActivatesAndSortsServers(t *testing.T) {
	first, firstServer := newNamedTestClient(t, "test", false)
	defer firstServer.Close()
	second, secondServer := newNamedTestClient(t, "zeta", false)
	defer secondServer.Close()
	pool := &Pool{clients: map[string]*Client{"test": first, "zeta": second}, names: []string{"zeta", "test"}}
	if err := pool.Activate(context.Background(), "test", "zeta"); err != nil {
		t.Fatal(err)
	}
	tools, err := pool.Tools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name() >= tools[1].Name() {
		t.Fatalf("tools = %#v", tools)
	}
	if err := pool.Health(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPoolConstructionAndNames(t *testing.T) {
	pool, err := NewPool([]ServerConfig{
		{Name: "zeta", Command: "zeta-server"},
		{Name: "alpha", Command: "alpha-server"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := pool.Names(); len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("names = %#v", got)
	}
	if _, ok := pool.Get("missing"); ok {
		t.Fatal("missing client found")
	}
	if err := pool.Add(ServerConfig{Name: "alpha", Command: "other"}); err == nil {
		t.Fatal("duplicate server accepted")
	}
	if err := pool.Activate(context.Background(), "missing"); err == nil {
		t.Fatal("missing activation accepted")
	}
	if err := pool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newTestClient(t *testing.T, fail bool) (*Client, *mcpsdk.ServerSession) {
	return newNamedTestClient(t, "test", fail)
}

func newNamedTestClient(t *testing.T, name string, fail bool) (*Client, *mcpsdk.ServerSession) {
	t.Helper()
	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test-server", Version: "1"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "echo",
		Description: "Echo a message.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"required": []string{"name"},
		},
	}, func(ctx context.Context, request *mcpsdk.CallToolRequest, input echoInput) (*mcpsdk.CallToolResult, any, error) {
		if fail {
			return &mcpsdk.CallToolResult{
				IsError: true,
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "requested failure"}},
			}, nil, nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "hello " + input.Name}},
		}, nil, nil
	})
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{Name: name, Command: "unused"})
	if err != nil {
		serverSession.Close()
		t.Fatal(err)
	}
	client.connectFn = func(ctx context.Context) (*mcpsdk.ClientSession, error) {
		return client.sdk.Connect(ctx, clientTransport, nil)
	}
	return client, serverSession
}
