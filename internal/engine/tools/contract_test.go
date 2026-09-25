// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
)

type panickingTool struct{}

func (panickingTool) Name() string { panic("tool name exploded") }

func (panickingTool) Description() string { return "Panicking tool." }

func (panickingTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (panickingTool) Execute(context.Context, json.RawMessage) (core.ToolResult, error) {
	return core.ToolResult{}, nil
}

type arraySchemaTool struct {
	*testTool
}

func (arraySchemaTool) Parameters() json.RawMessage { return json.RawMessage(`["not","an","object"]`) }

func TestPoolNilReceiverIsSafe(t *testing.T) {
	var pool *Pool
	valid := &testTool{name: "valid", description: "Valid tool.", execute: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{}, nil
	}}
	if err := pool.Register(valid); err == nil {
		t.Fatal("nil pool accepted a registration")
	}
	if err := pool.RegisterAll([]core.Tool{valid}); err == nil {
		t.Fatal("nil pool accepted RegisterAll")
	}
	if err := pool.Unregister("valid"); err == nil {
		t.Fatal("nil pool accepted Unregister")
	}
	if err := pool.SetEnabled("valid", true); err == nil {
		t.Fatal("nil pool accepted SetEnabled")
	}
	if err := pool.Enable("valid"); err == nil {
		t.Fatal("nil pool accepted Enable")
	}
	if err := pool.Disable("valid"); err == nil {
		t.Fatal("nil pool accepted Disable")
	}
	if tool, ok := pool.Get("valid"); tool != nil || ok {
		t.Fatal("nil pool returned a tool")
	}
	if tool, ok := pool.Lookup("valid"); tool != nil || ok {
		t.Fatal("nil pool returned a lookup result")
	}
	if len(pool.Tools()) != 0 || len(pool.AllTools()) != 0 || len(pool.Specs()) != 0 || len(pool.AllSpecs()) != 0 {
		t.Fatal("nil pool returned tool lists")
	}
	if _, err := pool.Execute(context.Background(), "valid", nil); err == nil {
		t.Fatal("nil pool executed a tool")
	}
}

func TestPoolRegistrationGuards(t *testing.T) {
	pool := NewPool()
	if err := pool.Register(nil); err == nil {
		t.Fatal("nil tool accepted")
	}
	if err := pool.Register(panickingTool{}); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("panicking tool error = %v", err)
	}
	if err := pool.Register(arraySchemaTool{&testTool{name: "array", description: "Array schema.", execute: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{}, nil
	}}}); err == nil {
		t.Fatal("array parameters accepted")
	}
	valid := &testTool{name: "valid", description: "Valid tool.", execute: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{}, nil
	}}
	if err := pool.RegisterAll([]core.Tool{valid}); err != nil {
		t.Fatal(err)
	}
	if err := pool.RegisterAll([]core.Tool{valid}); err == nil {
		t.Fatal("RegisterAll did not propagate the duplicate error")
	}
	if err := pool.Unregister("   "); err == nil {
		t.Fatal("blank name accepted by Unregister")
	}
	if err := pool.Unregister("absent"); err == nil {
		t.Fatal("unknown tool unregistered")
	}
	if err := pool.SetEnabled("   ", true); err == nil {
		t.Fatal("blank name accepted by SetEnabled")
	}
	if err := pool.SetEnabled("absent", true); err == nil {
		t.Fatal("unknown tool enabled")
	}
	if err := pool.Enable(); err == nil {
		t.Fatal("empty pattern list accepted")
	}
	if err := pool.Enable("  "); err == nil {
		t.Fatal("blank pattern accepted")
	}
	if _, err := pool.Execute(nil, "valid", nil); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := pool.Execute(context.Background(), "absent", nil); err == nil {
		t.Fatal("unknown tool executed")
	}
	if isJSONObject(json.RawMessage(`null`)) {
		t.Fatal("null accepted as a JSON object")
	}
	if isJSONObject(json.RawMessage(`{`)) || isJSONObject(nil) {
		t.Fatal("malformed parameters accepted")
	}
}

func TestWorkspaceRejectsInvalidConfiguration(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		root    string
		options WorkspaceOptions
	}{
		{name: "nul root", root: "bad\x00root"},
		{name: "missing root", root: filepath.Join(root, "missing")},
		{name: "file root", root: file},
		{name: "negative file limit", root: root, options: WorkspaceOptions{MaxFileBytes: -1}},
		{name: "negative result limit", root: root, options: WorkspaceOptions{MaxResults: -1}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewWorkspace(test.root, test.options); err == nil {
				t.Fatal("invalid workspace accepted")
			}
		})
	}
}

func TestNilWorkspaceIsRejectedEverywhere(t *testing.T) {
	var workspace *Workspace
	if workspace.Root() != "" || workspace.BuiltinTools() != nil {
		t.Fatal("nil workspace returned metadata")
	}
	if _, err := workspace.resolveRead("a.txt"); err == nil {
		t.Fatal("nil workspace resolved a read")
	}
	if _, err := workspace.resolveDirectory("."); err == nil {
		t.Fatal("nil workspace resolved a directory")
	}
	if _, err := workspace.prepareWrite("a.txt"); err == nil {
		t.Fatal("nil workspace prepared a write")
	}
	if _, err := workspace.readFile(context.Background(), "a.txt"); err == nil {
		t.Fatal("nil workspace read a file")
	}
	if err := workspace.writeFile(context.Background(), "a.txt", nil); err == nil {
		t.Fatal("nil workspace wrote a file")
	}
	if err := contextError(nil); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestWorkspacePathResolutionErrors(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{MaxFileBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err == nil {
		defer os.Remove(filepath.Join(root, "escape"))
	}
	if _, err := workspace.resolveRead("   "); err == nil {
		t.Fatal("blank path accepted")
	}
	if _, err := workspace.resolveRead("bad\x00path"); err == nil {
		t.Fatal("NUL path accepted")
	}
	if _, err := workspace.resolveRead("escape"); err == nil {
		t.Fatal("symlink escape accepted for reading")
	}
	if _, err := workspace.resolveRead("dir"); err == nil {
		t.Fatal("directory accepted as a regular file")
	}
	if _, err := workspace.resolveDirectory("small.txt"); err == nil {
		t.Fatal("file accepted as a directory")
	}
	if _, err := workspace.resolveDirectory("escape"); err == nil {
		t.Fatal("symlink escape accepted for directories")
	}
	if _, err := workspace.prepareWrite("dir"); err == nil {
		t.Fatal("directory accepted as a write target")
	}
	if _, err := workspace.prepareWrite("small.txt/child.txt"); err == nil {
		t.Fatal("file component accepted as a directory")
	}
	oversized, err := workspace.resolveRead("large.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.readFile(context.Background(), oversized); err == nil {
		t.Fatal("oversized file accepted for reading")
	}
	if _, err := workspace.readFile(context.Background(), filepath.Join(root, "missing.txt")); err == nil {
		t.Fatal("missing file read")
	}
	if err := workspace.writeFile(context.Background(), "large.txt", []byte("0123456789")); err == nil {
		t.Fatal("oversized content accepted for writing")
	}
	if err := workspace.writeFile(context.Background(), "../escape.txt", nil); err == nil {
		t.Fatal("escaping write accepted")
	}
	if got := workspace.relative(filepath.Join(root, "dir", "x.txt")); got != "dir/x.txt" {
		t.Fatalf("relative = %q", got)
	}
	if got := mustJoin(root, "a.txt"); got != filepath.Join(root, "a.txt") {
		t.Fatalf("mustJoin relative = %q", got)
	}
	if got := mustJoin(root, filepath.Join(root, "b.txt")); got != filepath.Join(root, "b.txt") {
		t.Fatalf("mustJoin absolute = %q", got)
	}
}

func TestWorkspaceWriteReplacesExistingFile(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "script.sh")
	if err := os.WriteFile(target, []byte("echo one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := workspace.writeFile(context.Background(), target, []byte("echo two")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "echo two" {
		t.Fatalf("content = %q", data)
	}
	fresh := filepath.Join(root, "nested", "created.txt")
	if err := workspace.writeFile(context.Background(), fresh, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(fresh); err != nil || string(data) != "new" {
		t.Fatalf("created file = %q err = %v", data, err)
	}
}

func TestFileToolsRejectUninitializedAndMalformedInput(t *testing.T) {
	var uninitialized []core.Tool = []core.Tool{(*readTool)(nil), &readTool{}, &writeTool{}, &editTool{}, &globTool{}, &grepTool{}, (*shellTool)(nil)}
	for _, tool := range uninitialized {
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Fatalf("%T accepted execution while uninitialized", tool)
		}
	}
	workspace, err := NewWorkspace(t.TempDir(), WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tools := []core.Tool{NewReadTool(workspace), NewWriteTool(workspace), NewEditTool(workspace), NewGlobTool(workspace), NewGrepTool(workspace)}
	for _, tool := range tools {
		if _, err := tool.Execute(context.Background(), json.RawMessage(`{`)); err == nil {
			t.Fatalf("%T accepted invalid JSON", tool)
		}
		if _, err := tool.Execute(nil, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("%T accepted a nil context", tool)
		}
	}
	if err := decodeParams(nil, &readParams{}); err != nil {
		t.Fatalf("empty parameters rejected: %v", err)
	}
	if err := decodeParams(json.RawMessage(`{"offset":"text"}`), &readParams{}); err == nil {
		t.Fatal("type mismatch accepted")
	}
}

func TestReadToolSlicesLines(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lines.txt"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := NewReadTool(workspace)
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":"lines.txt","offset":-1}`)); err == nil {
		t.Fatal("negative offset accepted")
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":"lines.txt","limit":-1}`)); err == nil {
		t.Fatal("negative limit accepted")
	}
	if _, err := read.Execute(context.Background(), json.RawMessage(`{"path":"missing.txt"}`)); err == nil {
		t.Fatal("missing file read")
	}
	result, err := read.Execute(context.Background(), json.RawMessage(`{"path":"lines.txt","offset":99}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Output), `"offset":4`) || !strings.Contains(string(result.Output), `"line_count":0`) {
		t.Fatalf("clamped read = %s", result.Output)
	}
}

func TestEditToolEnforcesUniqueMatches(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dup.txt"), []byte("x\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edit := NewEditTool(workspace)
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"dup.txt","old_string":"","new_string":"y"}`)); err == nil {
		t.Fatal("empty old_string accepted")
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"dup.txt","old_string":"missing","new_string":"y"}`)); err == nil {
		t.Fatal("missing match accepted")
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"dup.txt","old_string":"x","new_string":"y"}`)); err == nil {
		t.Fatal("ambiguous match accepted")
	}
	result, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"dup.txt","old_string":"x","new_string":"y","replace_all":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Output), `"replaced":2`) {
		t.Fatalf("replace_all output = %s", result.Output)
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"missing.txt","old_string":"a","new_string":"b"}`)); err == nil {
		t.Fatal("missing file edited")
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"dup.txt","old_string":"y","new_string":"z"}`)); err == nil {
		t.Fatal("missing file prepared for edit")
	}
}

func TestEditToolRejectsOversizedResult(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{MaxFileBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEditTool(workspace).Execute(context.Background(), json.RawMessage(`{"path":"small.txt","old_string":"aaaa","new_string":"aaaaaaaaaaaaaaaaaaaa"}`)); err == nil {
		t.Fatal("oversized edit accepted")
	}
}

func TestEditLineEndingHelpers(t *testing.T) {
	if replacementCount(true) != -1 || replacementCount(false) != 1 {
		t.Fatal("replacement count is incorrect")
	}
	if got := normalizeEditLineEndings("a\r\nb"); got != "a\r\nb" {
		t.Fatalf("already normalized = %q", got)
	}
	if got := normalizeEditLineEndings("a\nb"); got != "a\r\nb" {
		t.Fatalf("normalized = %q", got)
	}
}

func TestSearchToolsRejectUnsafePatterns(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	glob := NewGlobTool(workspace)
	grep := NewGrepTool(workspace)
	if _, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"   "}`)); err == nil {
		t.Fatal("blank glob accepted")
	}
	if _, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"/etc/passwd"}`)); err == nil {
		t.Fatal("absolute glob accepted")
	}
	if _, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"../outside/*"}`)); err == nil {
		t.Fatal("escaping glob accepted")
	}
	if _, err := glob.Execute(context.Background(), json.RawMessage(`{"pattern":"*.go","path":"missing"}`)); err == nil {
		t.Fatal("glob accepted a missing base directory")
	}
	if _, err := grep.Execute(context.Background(), json.RawMessage(`{"pattern":"a","glob":"/etc/*"}`)); err == nil {
		t.Fatal("absolute grep glob accepted")
	}
	if _, err := grep.Execute(context.Background(), json.RawMessage(`{"pattern":"a","glob":"../*"}`)); err == nil {
		t.Fatal("escaping grep glob accepted")
	}
	if _, err := grep.Execute(context.Background(), json.RawMessage(`{"pattern":"a","path":"missing"}`)); err == nil {
		t.Fatal("grep accepted a missing base directory")
	}
	if err := validateGlobPattern("**/[a-"); err == nil {
		t.Fatal("invalid glob segment accepted")
	}
	if !matchGlob("a/**/c", "a/b/c") || matchGlob("a/b", "a") {
		t.Fatal("glob matching is incorrect")
	}
}

func TestSearchToolsTruncateAndSkipBinary(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{MaxResults: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("needle\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte("needle\x00tail"), 0o600); err != nil {
		t.Fatal(err)
	}
	globResult, err := NewGlobTool(workspace).Execute(context.Background(), json.RawMessage(`{"pattern":"*.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(globResult.Output), `"truncated":true`) {
		t.Fatalf("glob output = %s", globResult.Output)
	}
	grepResult, err := NewGrepTool(workspace).Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(grepResult.Output), `"truncated":true`) {
		t.Fatalf("grep output = %s", grepResult.Output)
	}
	if strings.Contains(string(grepResult.Output), "binary.bin") {
		t.Fatalf("binary file was searched: %s", grepResult.Output)
	}
	limited, err := NewGrepTool(workspace).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","glob":"*.txt","max_matches":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(limited.Output), `"text"`) != 1 {
		t.Fatalf("max_matches ignored: %s", limited.Output)
	}
}

func TestSearchToolsSkipSymlinks(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	result, err := NewGlobTool(workspace).Execute(context.Background(), json.RawMessage(`{"pattern":"**/*"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result.Output), "link") {
		t.Fatalf("symlink was traversed: %s", result.Output)
	}
}

func TestGrepPropagatesUnreadableFiles(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, WorkspaceOptions{MaxFileBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte("needle in a large file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGrepTool(workspace).Execute(context.Background(), json.RawMessage(`{"pattern":"needle","glob":"*.txt"}`)); err == nil {
		t.Fatal("oversized file was silently skipped by grep")
	}
}

func TestDefaultStringFallsBack(t *testing.T) {
	if defaultString("  ", "fallback") != "fallback" || defaultString("value", "fallback") != "value" {
		t.Fatal("defaultString is incorrect")
	}
}

type stubProcess struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	result sandbox.Result
	err    error
}

func (p *stubProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *stubProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *stubProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *stubProcess) Wait() (sandbox.Result, error) {
	return p.result, p.err
}
func (p *stubProcess) Close() error { return nil }

type stubShellRunner struct {
	result sandbox.Result
	err    error
}

func (r stubShellRunner) Start(context.Context, []string, []string, string) (sandbox.Process, error) {
	return &stubProcess{result: r.result, err: r.err}, nil
}

func (stubShellRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{Backend: "stub", DefaultDeny: true}
}

func TestShellToolMetadataAndConfigurationGuards(t *testing.T) {
	tool, err := NewShellTool(ShellConfig{Sandbox: sandbox.NewUnrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	if tool.Name() != "bash" || tool.Description() == "" || !isJSONObject(tool.Parameters()) {
		t.Fatalf("shell metadata = %#v", tool)
	}
	if _, err := NewShellTool(ShellConfig{Sandbox: sandbox.NewUnrestricted(), Shell: "bad\x00shell"}); err == nil {
		t.Fatal("NUL shell accepted")
	}
	if _, err := NewShellTool(ShellConfig{Sandbox: sandbox.NewUnrestricted(), WorkingDirectory: "bad\x00dir"}); err == nil {
		t.Fatal("NUL working directory accepted")
	}
	if _, err := NewShellTool(ShellConfig{Sandbox: sandbox.NewUnrestricted(), Env: []string{"A=bad\x00value"}}); err == nil {
		t.Fatal("NUL environment accepted")
	}
	if _, err := NewShellTool(ShellConfig{Sandbox: sandbox.NewUnrestricted(), MaxOutputBytes: -1}); err == nil {
		t.Fatal("negative output limit accepted")
	}
	workspace := t.TempDir()
	if _, err := NewShellTool(ShellConfig{Sandbox: sandbox.NewUnrestricted(), WorkingDirectory: workspace}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewShellTool(ShellConfig{Sandbox: sandbox.NewUnrestricted(), WorkingDirectory: file}); err == nil {
		t.Fatal("file working directory accepted")
	}
}

func TestShellToolPropagatesStartFailures(t *testing.T) {
	tool, err := NewShellTool(ShellConfig{Shell: "shell", MaxOutputBytes: 4, Sandbox: stubShellRunner{err: errors.New("boom")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"noop"}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("start failure = %v", err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"   "}`)); err == nil {
		t.Fatal("blank command accepted")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid parameters accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(canceled, json.RawMessage(`{"command":"noop"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestShellToolReportsStubbedFailureExit(t *testing.T) {
	tool, err := NewShellTool(ShellConfig{Shell: "shell", MaxOutputBytes: 4, Sandbox: stubShellRunner{result: sandbox.Result{ExitCode: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"noop"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || result.Error == "" || !strings.Contains(string(result.Output), `"exit_code":4`) {
		t.Fatalf("result = %#v", result)
	}
}

func TestCappedBufferLimitsWrites(t *testing.T) {
	buffer := &cappedBuffer{limit: 4}
	if written, err := buffer.Write([]byte("ab")); err != nil || written != 2 || buffer.Len() != 2 {
		t.Fatalf("short write = %d err = %v len = %d", written, err, buffer.Len())
	}
	if written, err := buffer.Write([]byte("cdef")); err != nil || written != 4 {
		t.Fatalf("clamped write = %d err = %v", written, err)
	}
	if buffer.String() != "abcd" || !buffer.truncated {
		t.Fatalf("buffer = %q truncated = %v", buffer.String(), buffer.truncated)
	}
	if written, err := buffer.Write([]byte("g")); err != nil || written != 1 {
		t.Fatalf("saturated write = %d err = %v", written, err)
	}
	if buffer.String() != "abcd" {
		t.Fatalf("buffer grew past the limit: %q", buffer.String())
	}
}

func TestRegisterAllRejectsNilSliceElements(t *testing.T) {
	pool := NewPool()
	if err := pool.RegisterAll([]core.Tool{nil}); err == nil {
		t.Fatal("nil tool in RegisterAll accepted")
	}
	if len(pool.AllTools()) != 0 {
		t.Fatal("failed registration mutated the pool")
	}
}

func TestExecuteErrorsAreDescriptive(t *testing.T) {
	pool := NewPool()
	tool := &testTool{name: "boom", description: "Boom tool.", execute: func(context.Context, json.RawMessage) (core.ToolResult, error) {
		return core.ToolResult{}, fmt.Errorf("tool exploded")
	}}
	if err := pool.RegisterEnabled(tool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Execute(context.Background(), "boom", nil); err == nil || !strings.Contains(err.Error(), "exploded") {
		t.Fatalf("execute error = %v", err)
	}
}
