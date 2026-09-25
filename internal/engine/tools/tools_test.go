// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
)

type testTool struct {
	name        string
	description string
	execute     func(context.Context, json.RawMessage) (core.ToolResult, error)
}

func (t *testTool) Name() string {
	return t.name
}

func (t *testTool) Description() string {
	return t.description
}

func (t *testTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *testTool) Execute(ctx context.Context, params json.RawMessage) (core.ToolResult, error) {
	return t.execute(ctx, params)
}

func TestPoolRegistersStableEnabledTools(t *testing.T) {
	pool := NewPool()
	first := &testTool{name: "zeta", description: "Zeta tool.", execute: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{Output: json.RawMessage(`"zeta"`)}, nil
	}}
	second := &testTool{name: "alpha", description: "Alpha tool.", execute: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{Output: json.RawMessage(`"alpha"`)}, nil
	}}
	if err := pool.Register(first); err != nil {
		t.Fatal(err)
	}
	if err := pool.RegisterEnabled(second); err != nil {
		t.Fatal(err)
	}
	if len(pool.Tools()) != 1 || pool.Tools()[0].Name() != "alpha" {
		t.Fatalf("enabled tools = %#v", pool.Tools())
	}
	if len(pool.AllTools()) != 2 {
		t.Fatalf("all tools = %#v", pool.AllTools())
	}
	if err := pool.Enable("*a"); err != nil {
		t.Fatal(err)
	}
	tools := pool.Tools()
	if len(tools) != 2 || tools[0].Name() != "alpha" || tools[1].Name() != "zeta" {
		t.Fatalf("sorted tools = %#v", tools)
	}
	if len(pool.Specs()) != 2 || pool.Specs()[0].Name != "alpha" {
		t.Fatalf("specs = %#v", pool.Specs())
	}
	result, err := pool.Execute(context.Background(), "zeta", json.RawMessage(`{}`))
	if err != nil || string(result.Output) != `"zeta"` {
		t.Fatalf("execute result = %#v, err = %v", result, err)
	}
	if err := pool.Disable("alpha"); err != nil {
		t.Fatal(err)
	}
	if pool.IsEnabled("alpha") {
		t.Fatal("alpha remains enabled")
	}
	if err := pool.Unregister("zeta"); err != nil {
		t.Fatal(err)
	}
	if _, ok := pool.Lookup("zeta"); ok {
		t.Fatal("unregistered tool remains")
	}
}

func TestPoolRejectsInvalidRegistrationsAndPatterns(t *testing.T) {
	pool := NewPool()
	if err := pool.Register(&testTool{name: "bad", description: "", execute: func(context.Context, json.RawMessage) (core.ToolResult, error) { return core.ToolResult{}, nil }}); err == nil {
		t.Fatal("empty description accepted")
	}
	valid := &testTool{name: "valid", description: "Valid tool.", execute: func(context.Context, json.RawMessage) (core.ToolResult, error) { return core.ToolResult{}, nil }}
	if err := pool.Register(valid); err != nil {
		t.Fatal(err)
	}
	if err := pool.Register(valid); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := pool.Enable("missing"); err == nil {
		t.Fatal("unmatched pattern accepted")
	}
	if err := pool.Enable("["); err == nil {
		t.Fatal("invalid pattern accepted")
	}
	if err := pool.Enable("valid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Execute(context.Background(), "valid", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFileTools(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	write := NewWriteTool(workspace)
	edit := NewEditTool(workspace)
	read := NewReadTool(workspace)
	result, err := write.Execute(context.Background(), json.RawMessage(`{"path":"src/a.txt","content":"one\r\ntwo\r\nthree\r\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Output), `"bytes":17`) {
		t.Fatalf("write output = %s", result.Output)
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"src/a.txt","old_string":"two","new_string":"TWO"}`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "src", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\r\nTWO\r\nthree\r\n" {
		t.Fatalf("edited data = %q", data)
	}
	readResult, err := read.Execute(context.Background(), json.RawMessage(`{"path":"src/a.txt","offset":1,"limit":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readResult.Output), `"content":"TWO\r"`) || !strings.Contains(string(readResult.Output), `"truncated":true`) {
		t.Fatalf("read output = %s", readResult.Output)
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":"../outside.txt"}`)); err == nil {
		t.Fatal("workspace escape accepted")
	}
	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":"../outside.txt","content":"bad"}`)); err == nil {
		t.Fatal("workspace write escape accepted")
	}
}

func TestSearchTools(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"main.go":      "package main\nneedle\n",
		"docs/read.md": "needle in docs\n",
		"ignore.txt":   "plain\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	glob := NewGlobTool(workspace)
	globResult, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(globResult.Output), `"main.go"`) || strings.Contains(string(globResult.Output), "read.md") {
		t.Fatalf("glob output = %s", globResult.Output)
	}
	grep := NewGrepTool(workspace)
	grepResult, err := grep.Execute(context.Background(), json.RawMessage(`{"pattern":"needle","glob":"**/*"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(grepResult.Output), `"path"`) != 2 {
		t.Fatalf("grep output = %s", grepResult.Output)
	}
}

func TestWorkspaceRejectsSymlinkWriteAndInvalidRoots(t *testing.T) {
	if _, err := NewWorkspace("", WorkspaceOptions{}); err == nil {
		t.Fatal("empty root accepted")
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err == nil {
		defer os.Remove(filepath.Join(root, "link"))
	}
	workspace, err := NewWorkspace(root, WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriteTool(workspace).Execute(context.Background(), json.RawMessage(`{"path":"link/file.txt","content":"bad"}`)); err == nil {
		t.Fatal("symlink write accepted")
	}
}

func TestBuiltinToolMetadataAndPoolComposition(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	builtin := workspace.BuiltinTools()
	if len(builtin) != 5 {
		t.Fatalf("builtin tools = %d", len(builtin))
	}
	pool := NewPool()
	if err := pool.RegisterAll(builtin); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetEnabled("read", true); err != nil {
		t.Fatal(err)
	}
	if len(pool.Specs()) != 1 || pool.Specs()[0].Name != "read" {
		t.Fatalf("enabled specs = %#v", pool.Specs())
	}
	if len(pool.AllSpecs()) != 5 {
		t.Fatalf("all specs = %#v", pool.AllSpecs())
	}
	for _, tool := range builtin {
		if tool.Name() == "" || tool.Description() == "" || !isJSONObject(tool.Parameters()) {
			t.Fatalf("invalid metadata for %#v", tool)
		}
	}
	if _, err := pool.Execute(context.Background(), "write", json.RawMessage(`{}`)); err == nil {
		t.Fatal("disabled tool executed")
	}
}

func TestToolInputValidationAndCancellation(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), WorkspaceOptions{MaxFileBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewReadTool(workspace).Execute(ctx, json.RawMessage(`{"path":"missing"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation = %v", err)
	}
	if _, err := NewWriteTool(workspace).Execute(context.Background(), json.RawMessage(`{"path":"a.txt","content":"12345"}`)); err == nil {
		t.Fatal("oversized write accepted")
	}
	if _, err := NewGlobTool(workspace).Execute(context.Background(), json.RawMessage(`{"pattern":"["}`)); err == nil {
		t.Fatal("invalid glob accepted")
	}
	if _, err := NewGrepTool(workspace).Execute(context.Background(), json.RawMessage(`{"pattern":"["}`)); err == nil {
		t.Fatal("invalid grep expression accepted")
	}
	if _, err := NewShellTool(ShellConfig{}); !errors.Is(err, sandbox.ErrRequired) {
		t.Fatalf("missing sandbox error = %v", err)
	}
	if _, err := NewShellTool(ShellConfig{WorkingDirectory: filepath.Join(workspace.Root(), "missing"), Sandbox: sandbox.NewUnrestricted()}); err == nil {
		t.Fatal("invalid shell directory accepted")
	}
}
