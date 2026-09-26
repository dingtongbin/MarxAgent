// SPDX-License-Identifier: Apache-2.0

package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// newTooledPool builds a pool over a fake server that answers every question, so
// the tools are tested against the protocol rather than against a stub.
func newTooledPool(t *testing.T) *Pool {
	t.Helper()
	starter := workingStarter(t)
	client, err := New(Config{
		Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter,
		RequestTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	pool, err := NewPool(client)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

// findTool returns the tool with a name.
func findTool(t *testing.T, pool *Pool, name string) core.Tool {
	t.Helper()
	for _, tool := range pool.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("there is no %s tool", name)
	return nil
}

func call(t *testing.T, tool core.Tool, params map[string]any) (map[string]any, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), raw)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		t.Fatalf("output is not json: %s (%v)", result.Output, err)
	}
	return payload, nil
}

func TestPoolExposesTheFiveToolsInAStableOrder(t *testing.T) {
	pool := newTooledPool(t)
	// The order is fixed because the tool declarations are part of the cached
	// prefix, and a pool that reordered them would invalidate every session's cache.
	want := []string{ToolDefinition, ToolReferences, ToolDiagnostics, ToolHover, ToolRename}
	for index, name := range want {
		tool := findTool(t, pool, name)
		if tool.Name() != want[index] {
			t.Fatalf("tool %d = %q, want %q", index, tool.Name(), want[index])
		}
		// A description and a schema are not optional: a model chooses a tool by
		// reading them.
		description := tool.Description()
		if len(description) < 40 {
			t.Fatalf("%s has a description of %d characters", name, len(description))
		}
		if strings.ContainsAny(description, "äöü") {
			t.Fatalf("%s has a description that is not standard english: %q", name, description)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
			t.Fatalf("%s has a schema that is not json: %v", name, err)
		}
		if schema["type"] != "object" {
			t.Fatalf("%s schema = %v", name, schema)
		}
		// Every tool but the diagnostics one has something it must be given, and a
		// schema that does not say so leaves a model to guess.
		if name != ToolDiagnostics {
			if _, hasRequired := schema["required"]; !hasRequired {
				t.Fatalf("%s does not say what it requires", name)
			}
		}
	}
	// The slice the caller holds is a copy, so it cannot change the pool.
	tools := pool.Tools()
	tools[0] = nil
	if pool.Tools()[0] == nil {
		t.Fatal("the caller mutated the pool's tools")
	}
	if _, err := NewPool(nil); err == nil {
		t.Fatal("a pool without a client was accepted")
	}
	if pool.Client() == nil {
		t.Fatal("the pool reports no client")
	}
}

func TestHoverReportsWhatTheServerSays(t *testing.T) {
	pool := newTooledPool(t)
	payload, err := call(t, findTool(t, pool, ToolHover), map[string]any{
		"path": "/project/main.go", "line": 10, "column": 6,
		"contents": "package main\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	text, _ := payload["text"].(string)
	if !strings.Contains(text, "func Add") {
		t.Fatalf("text = %q", text)
	}
}

func TestHoverSaysSoWhenTheServerHasNothing(t *testing.T) {
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		server := newFakeServer(t)
		server.handler = func(method string, _ json.RawMessage) (json.RawMessage, *ResponseError) {
			if method == MethodTextDocumentHover {
				return json.RawMessage("null"), nil
			}
			return nil, &ResponseError{Code: CodeMethodNotFound, Message: method}
		}
		server.attach(process.server, process.client)
	}}
	client, err := New(Config{Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pool, err := NewPool(client)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := call(t, findTool(t, pool, ToolHover), map[string]any{
		"path": "/project/main.go", "line": 1, "column": 1, "contents": "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := payload["text"].(string); !strings.Contains(text, "nothing to say") {
		t.Fatalf("text = %q", text)
	}
	// A hover that answers with an empty value is the same case.
	starter.track(t, func(process *pipeProcess) *fakeServer {
		server := newFakeServer(t)
		server.handler = func(method string, _ json.RawMessage) (json.RawMessage, *ResponseError) {
			if method == MethodTextDocumentHover {
				return json.RawMessage(`{"contents":{"kind":"markdown","value":"  "}}`), nil
			}
			return nil, &ResponseError{Code: CodeMethodNotFound, Message: method}
		}
		server.attach(process.server, process.client)

		return server
	})
	// A second client is needed because the first one already negotiated its
	// capabilities, and the assertion is about the empty value rather than the
	// negotiation.
	second, err := New(Config{Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondPool, err := NewPool(second)
	if err != nil {
		t.Fatal(err)
	}
	payload, err = call(t, findTool(t, secondPool, ToolHover), map[string]any{
		"path": "/project/main.go", "line": 1, "column": 1, "contents": "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := payload["text"].(string); !strings.Contains(text, "nothing to say") {
		t.Fatalf("text = %q", text)
	}
}

func TestDefinitionAndReferencesAreReportedWithCounts(t *testing.T) {
	pool := newTooledPool(t)
	for _, name := range []string{ToolDefinition, ToolReferences} {
		payload, err := call(t, findTool(t, pool, name), map[string]any{
			"path": "/project/main.go", "line": 10, "column": 6, "contents": "package main\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		if payload["count"] != float64(1) {
			t.Fatalf("%s count = %v", name, payload["count"])
		}
		text, _ := payload["text"].(string)
		if !strings.Contains(text, "main.go:10:6") {
			t.Fatalf("%s text = %q", name, text)
		}
	}
	// The references tool also reports how many files the answer touches, because
	// that is the number a caller decides on.
	payload, err := call(t, findTool(t, pool, ToolReferences), map[string]any{
		"path": "/project/main.go", "line": 10, "column": 6, "contents": "package main\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload["files"] != float64(1) {
		t.Fatalf("files = %v", payload["files"])
	}
}

func TestRenameReturnsEditsAndWritesNothing(t *testing.T) {
	pool := newTooledPool(t)
	payload, err := call(t, findTool(t, pool, ToolRename), map[string]any{
		"path": "/project/main.go", "line": 10, "column": 6,
		"new_name": "Sum", "contents": "package main\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload["applied"] != false {
		t.Fatal("the rename tool claims it applied something")
	}
	if payload["edits"] != float64(1) || payload["files"] != float64(1) {
		t.Fatalf("payload = %v", payload)
	}
	if payload["new_name"] != "Sum" {
		t.Fatalf("new_name = %v", payload["new_name"])
	}
	text, _ := payload["text"].(string)
	var edit WorkspaceEdit
	if err := json.Unmarshal([]byte(text), &edit); err != nil {
		t.Fatalf("text is not an edit set: %q", text)
	}
	if len(edit.Changes) != 1 {
		t.Fatalf("edits = %#v", edit.Changes)
	}
}

func TestACapabilityTheServerLacksIsRefused(t *testing.T) {
	// Sending a request for a capability the server did not claim would only earn
	// an error, and an error a caller has to interpret is worse than a refusal.
	starter := &pipeStarter{onLaunch: func(process *pipeProcess) {
		server := newFakeServer(t)
		server.capabilities = ServerCapabilities{}
		server.attach(process.server, process.client)
	}}
	client, err := New(Config{Command: []string{"gopls"}, Workspace: t.TempDir(), Starter: starter})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pool, err := NewPool(client)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ToolDefinition, ToolReferences, ToolHover, ToolRename} {
		params := map[string]any{
			"path": "/project/main.go", "line": 1, "column": 1, "contents": "x",
		}
		if name == ToolRename {
			params["new_name"] = "Other"
		}
		_, err := call(t, findTool(t, pool, name), params)
		if err == nil {
			t.Fatalf("%s was called on a server that does not offer it", name)
		}
		if !strings.Contains(err.Error(), "does not provide") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestPositionParametersAreCheckedBeforeAnythingIsSent(t *testing.T) {
	pool := newTooledPool(t)
	hover := findTool(t, pool, ToolHover)
	cases := []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{"no path", map[string]any{"line": 1, "column": 1}, "a path is required"},
		{"blank path", map[string]any{"path": "  ", "line": 1, "column": 1}, "a path is required"},
		{"zero line", map[string]any{"path": "/a.go", "line": 0, "column": 1}, "one based"},
		{"zero column", map[string]any{"path": "/a.go", "line": 1, "column": 0}, "one based"},
		{"negative", map[string]any{"path": "/a.go", "line": -1, "column": -1}, "one based"},
		{"never opened", map[string]any{"path": "/a.go", "line": 1, "column": 1}, "has not been opened"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := call(t, hover, testCase.params)
			if err == nil {
				t.Fatal("the parameters were accepted")
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, testCase.wantErr)
			}
		})
	}
	// Parameters that are not json at all are reported rather than half read.
	raw := json.RawMessage(`{"path":`)
	if _, err := hover.Execute(context.Background(), raw); err == nil {
		t.Fatal("malformed parameters were accepted")
	}
	if _, err := hover.Execute(context.Background(), nil); err == nil {
		t.Fatal("a call with no parameters was accepted")
	}
	// A rename with no new name is refused, because renaming something to nothing
	// is never what a caller meant.
	_, err := call(t, findTool(t, pool, ToolRename), map[string]any{
		"path": "/a.go", "line": 1, "column": 1, "new_name": "  ", "contents": "x",
	})
	if err == nil || !strings.Contains(err.Error(), "new name") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpeningADocumentVersionsIt(t *testing.T) {
	pool := newTooledPool(t)
	path := "/project/main.go"
	if err := pool.Open(path, "package main\n"); err != nil {
		t.Fatal(err)
	}
	if got := pool.documentVersion(PathToURI(path)); got != FirstDocumentVersion {
		t.Fatalf("version = %d", got)
	}
	// A second open is a change, and the version has to advance or the server will
	// reject an answer about a version it has already replaced.
	if err := pool.Open(path, "package main // edited\n"); err != nil {
		t.Fatal(err)
	}
	if got := pool.documentVersion(PathToURI(path)); got != FirstDocumentVersion+1 {
		t.Fatalf("version = %d", got)
	}
	if err := pool.Open("  ", "x"); err == nil {
		t.Fatal("a blank path was opened")
	}
	opened := pool.Opened()
	if len(opened) != 1 || opened[0] != PathToURI(path) {
		t.Fatalf("opened = %v", opened)
	}
	if err := pool.CloseDocument(path); err != nil {
		t.Fatal(err)
	}
	if len(pool.Opened()) != 0 {
		t.Fatalf("opened = %v", pool.Opened())
	}
}

func TestDiagnosticsReadsTheStoreRatherThanAsking(t *testing.T) {
	pool := newTooledPool(t)
	store := pool.Client().Diagnostics()
	// Real paths, because an absolute path means something different on each
	// platform and the report has to name the same files the store holds.
	first := filepath.Join(t.TempDir(), "first.go")
	second := filepath.Join(t.TempDir(), "second.go")
	store.Replace(PathToURI(first), []Diagnostic{
		{Range: Range{Start: Position{Line: 9, Character: 4}}, Severity: SeverityError,
			Message: "undefined: foo"},
		{Range: Range{Start: Position{Line: 20, Character: 0}}, Severity: SeverityWarning,
			Message: "unused variable"},
	})
	store.Replace(PathToURI(second), []Diagnostic{
		{Range: Range{Start: Position{Line: 4, Character: 4}}, Severity: SeverityError,
			Message: "the second file"},
	})
	tool := findTool(t, pool, ToolDiagnostics)

	// One file. Scoping is the point: the other file's problems are not reported,
	// because a caller that named a file asked about that file.
	payload, err := call(t, tool, map[string]any{"path": first})
	if err != nil {
		t.Fatal(err)
	}
	if payload["count"] != float64(2) {
		t.Fatalf("count = %v", payload["count"])
	}
	text, _ := payload["text"].(string)
	if !strings.Contains(text, "first.go:10:5: error: undefined: foo") {
		t.Fatalf("text = %q", text)
	}
	// The coordinates are one based because that is how a diagnostic is quoted
	// everywhere else, and a report on a different base would be a trap.
	if !strings.Contains(text, "first.go:21:1: warning: unused variable") {
		t.Fatalf("text = %q", text)
	}
	if strings.Contains(text, "the second file") {
		t.Fatalf("a scoped report included another file: %q", text)
	}

	// Every open file.
	payload, err = call(t, tool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if payload["count"] != float64(3) {
		t.Fatalf("count = %v, want all three problems", payload["count"])
	}
	// A severity filter keeps the problems at or above it, out of everything held.
	payload, err = call(t, tool, map[string]any{"severity": "error"})
	if err != nil {
		t.Fatal(err)
	}
	if payload["reported"] != float64(2) {
		t.Fatalf("reported = %v, want the two errors", payload["reported"])
	}
	if strings.Contains(payload["text"].(string), "unused variable") {
		t.Fatalf("a warning survived an error only report: %q", payload["text"])
	}
	// The lowest severity keeps everything, because a caller that asked for hints
	// asked not to be told about anything else being wrong.
	payload, err = call(t, tool, map[string]any{"severity": "hint"})
	if err != nil {
		t.Fatal(err)
	}
	if payload["reported"] != float64(3) {
		t.Fatalf("reported = %v, want every problem", payload["reported"])
	}
	// A filter that leaves nothing says so, because a report with nothing in it is
	// indistinguishable from a clean file.
	kept, dropped := filterBySeverity([]string{
		"a.go:1:1: error: kept",
		"a.go:2:1: hint: dropped",
	}, "error")
	if len(kept) != 1 || dropped != 1 {
		t.Fatalf("kept = %v dropped = %d", kept, dropped)
	}
	if keptAll, noneDropped := filterBySeverity([]string{"a.go:1:1: error: x"}, "nonsense"); len(keptAll) != 1 || noneDropped != 0 {
		t.Fatalf("an unknown severity hid problems: %v %d", keptAll, noneDropped)
	}
	// Nothing wrong says so rather than producing an empty region.
	empty := findTool(t, newTooledPool(t), ToolDiagnostics)
	payload, err = call(t, empty, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload["text"].(string), "no problems") {
		t.Fatalf("text = %q", payload["text"])
	}
	// An unknown severity filters nothing, because hiding every problem silently is
	// the worse failure.
	payload, err = call(t, tool, map[string]any{"severity": "catastrophe"})
	if err != nil {
		t.Fatal(err)
	}
	if payload["reported"] != float64(3) {
		t.Fatalf("an unknown severity hid problems: %v", payload)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"severity":`)); err == nil {
		t.Fatal("malformed diagnostics parameters were accepted")
	}
}

func TestDiagnosticStoreReplacesRatherThanMerges(t *testing.T) {
	store := NewDiagnosticStore()
	uri := PathToURI("/project/main.go")
	store.Replace(uri, []Diagnostic{{Severity: SeverityError, Message: "one"}})
	// The protocol pushes a full list per document, so merging would keep a problem
	// the server has already fixed.
	store.Replace(uri, []Diagnostic{{Severity: SeverityWarning, Message: "two"}})
	diagnostics := store.For(uri)
	if len(diagnostics) != 1 || diagnostics[0].Message != "two" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	store.Replace(uri, nil)
	if store.Count(uri) != 0 {
		t.Fatal("an emptied list was not dropped")
	}
	store.Replace(uri, []Diagnostic{{Severity: SeverityError, Message: "kept"}})
	store.Forget(uri)
	if store.Total() != 0 {
		t.Fatal("a forgotten document still holds diagnostics")
	}
	if len(store.URIs()) != 0 {
		t.Fatal("a forgotten document is still listed")
	}
	// The copy the caller holds cannot change the store.
	store.Replace(uri, []Diagnostic{{Severity: SeverityError, Message: "mine"}})
	held := store.For(uri)
	held[0].Message = "mutated"
	if store.For(uri)[0].Message != "mine" {
		t.Fatal("the caller mutated the store's diagnostics")
	}
	if store.Total() != 1 {
		t.Fatalf("total = %d", store.Total())
	}
	if len(store.Snapshot()) != 1 {
		t.Fatalf("snapshot = %#v", store.Snapshot())
	}
	store.Clear()
	if store.Total() != 0 {
		t.Fatal("the store was not cleared")
	}
}

func TestDiagnosticStorePublishesFromTheWire(t *testing.T) {
	store := NewDiagnosticStore()
	params := mustMarshal(DiagnosticsParams{
		URI: "file:///a.go",
		Diagnostics: []Diagnostic{
			{Range: Range{Start: Position{Line: 1, Character: 1}}, Severity: SeverityError,
				Message: "broken"},
		},
	})
	if err := store.Publish(params); err != nil {
		t.Fatal(err)
	}
	if store.Count("file:///a.go") != 1 {
		t.Fatal("the published diagnostics did not land")
	}
	if err := store.Publish(json.RawMessage("{")); err == nil {
		t.Fatal("a malformed push was accepted")
	}
}

func TestErrorsAreTheOnesThatBreakABuild(t *testing.T) {
	store := NewDiagnosticStore()
	store.Replace("file:///a.go", []Diagnostic{
		{Severity: SeverityError, Message: "one"},
		{Severity: SeverityWarning, Message: "two"},
	})
	store.Replace("file:///b.go", []Diagnostic{{Severity: SeverityHint, Message: "three"}})
	errors := store.Errors()
	if len(errors) != 1 || errors[0].Message != "one" {
		t.Fatalf("errors = %#v", errors)
	}
}

func TestURIsAndPathsRoundTrip(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a file with spaces.go")
	uri := PathToURI(path)
	if !strings.HasPrefix(uri, "file://") {
		t.Fatalf("uri = %q", uri)
	}
	// A space has to be escaped, because a server that receives an unescaped uri
	// fails to open the file and reports a problem that is not one.
	if strings.Contains(uri, " ") {
		t.Fatalf("uri = %q", uri)
	}
	back, err := URIToPath(uri)
	if err != nil {
		t.Fatal(err)
	}
	if back != path {
		t.Fatalf("round trip = %q, want %q", back, path)
	}
	// A uri that is already a uri is left alone, so a caller can pass either form.
	if PathToURI(uri) != uri {
		t.Fatalf("uri = %q", PathToURI(uri))
	}
	if got := PathToURI(""); got != "" {
		t.Fatalf("empty path = %q", got)
	}
	if _, err := URIToPath(""); err == nil {
		t.Fatal("an empty uri was accepted")
	}
	if _, err := URIToPath("https://example.invalid/page"); err == nil {
		t.Fatal("a non file uri was accepted")
	}
	if _, err := URIToPath("file://host/share/file.go"); err != nil {
		t.Fatalf("a network share uri was rejected: %v", err)
	}
}

func TestLanguageForCoversTheLanguagesTheProjectUses(t *testing.T) {
	cases := map[string]string{
		"main.go":       "go",
		"main.GO":       "go",
		"lib.rs":        "rust",
		"app.py":        "python",
		"a.ts":          "typescript",
		"a.tsx":         "typescript",
		"a.js":          "javascript",
		"a.c":           "c",
		"a.hpp":         "cpp",
		"README.md":     "markdown",
		"config.yaml":   "yaml",
		"config.yml":    "yaml",
		"Cargo.toml":    "toml",
		"run.sh":        "shellscript",
		"notes.unknown": "plaintext",
		"Makefile":      "plaintext",
	}
	for path, want := range cases {
		if got := LanguageFor(path); got != want {
			t.Fatalf("LanguageFor(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestInjectorAppendsToTheTailAndOnlyWhenItHasSomething(t *testing.T) {
	pool := newTooledPool(t)
	injector, err := NewDiagnosticsInjector(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing wrong produces nothing: an empty region is still content in the
	// request, and a turn with nothing wrong should not pay for a region that says
	// so.
	message, count := injector.Inject()
	if message != nil || count != 0 {
		t.Fatalf("message = %#v count = %d", message, count)
	}
	request := core.ChatRequest{Messages: []core.Message{userMessage("a question")}}
	out, err := injector.Hook()(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, ok := out.(core.ChatRequest)
	if !ok {
		t.Fatalf("the hook returned a %T", out)
	}
	if len(unchanged.Messages) != 1 {
		t.Fatalf("the hook added a message with nothing to say: %#v", unchanged.Messages)
	}

	named := filepath.Join(t.TempDir(), "main.go")
	pool.Client().Diagnostics().Replace(PathToURI(named), []Diagnostic{{
		Range:    Range{Start: Position{Line: 2, Character: 3}},
		Severity: SeverityError,
		Message:  "undefined: foo",
	}})
	message, count = injector.Inject()
	if message == nil || count != 1 {
		t.Fatalf("message = %#v count = %d", message, count)
	}
	text := message.Content[0].Text
	if !strings.Contains(text, DiagnosticsTag) || !strings.Contains(text, DiagnosticsClose) {
		t.Fatalf("text = %q", text)
	}
	// The region says what it is, so a model can tell a report from an
	// instruction, and it names the tag so the model can point at it.
	if !strings.Contains(text, DiagnosticsNotice) {
		t.Fatalf("text does not carry the notice: %q", text)
	}
	if !strings.Contains(text, "line=3 column=4") {
		t.Fatalf("coordinates are not one based: %q", text)
	}
	if !strings.Contains(text, "file=") || !strings.Contains(text, "main.go") {
		t.Fatalf("text does not name the file: %q", text)
	}
	if message.Metadata["problems"] != 1 {
		t.Fatalf("metadata = %#v", message.Metadata)
	}

	// The hook appends rather than prepends, because the front is the cached
	// prefix and volatile content there would invalidate the cache every turn.
	out, err = injector.Hook()(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	appended := out.(core.ChatRequest)
	if len(appended.Messages) != 2 {
		t.Fatalf("messages = %d", len(appended.Messages))
	}
	if appended.Messages[0].Content[0].Text != "a question" {
		t.Fatal("the diagnostics were prepended into the cached prefix")
	}
	if appended.Messages[1].Metadata["source"] != "lsp_diagnostics" {
		t.Fatalf("metadata = %#v", appended.Messages[1].Metadata)
	}
	// The caller's request is untouched, so a caller that still holds it does not
	// find a message appended behind its back.
	if len(request.Messages) != 1 {
		t.Fatal("the hook wrote into the caller's request")
	}
	if injector.InjectedCount() != 1 {
		t.Fatalf("injected = %d", injector.InjectedCount())
	}
}

func TestInjectorTruncatesEvenlyAcrossFiles(t *testing.T) {
	pool := newTooledPool(t)
	injector, err := NewDiagnosticsInjector(pool, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"/project/a.go", "/project/b.go"} {
		diagnostics := make([]Diagnostic, 0, 6)
		for index := 0; index < 6; index++ {
			diagnostics = append(diagnostics, Diagnostic{
				Range:    Range{Start: Position{Line: index, Character: 0}},
				Severity: SeverityError,
				Message:  "problem " + string(rune('a'+index)),
			})
		}
		pool.Client().Diagnostics().Replace(PathToURI(name), diagnostics)
	}
	message, _ := injector.Inject()
	if message == nil {
		t.Fatal("nothing was injected")
	}
	// The budget is spread evenly, because a single broken file would otherwise
	// consume the whole report and the errors in the other file would go unseen.
	if !strings.Contains(message.Content[0].Text, "a.go") {
		t.Fatalf("the first file is missing: %q", message.Content[0].Text)
	}
	if !strings.Contains(message.Content[0].Text, "b.go") {
		t.Fatalf("the second file is missing: %q", message.Content[0].Text)
	}
	if message.Metadata["truncated"] == 0 {
		t.Fatalf("nothing was reported as truncated: %#v", message.Metadata)
	}
	if !strings.Contains(message.Content[0].Text, "further problems were not shown") {
		t.Fatalf("the truncation is not stated: %q", message.Content[0].Text)
	}
}

func TestInjectorFiltersBySeverityAndPath(t *testing.T) {
	pool := newTooledPool(t)
	injector, err := NewDiagnosticsInjector(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	store := pool.Client().Diagnostics()
	store.Replace(PathToURI("/project/a.go"), []Diagnostic{
		{Severity: SeverityError, Message: "a error"},
		{Severity: SeverityHint, Message: "a hint"},
	})
	store.Replace(PathToURI("/project/b.go"), []Diagnostic{
		{Severity: SeverityWarning, Message: "b warning"},
	})
	// Only errors.
	injector.SetSeverities(SeverityError)
	message, _ := injector.Inject()
	if message == nil || !strings.Contains(message.Content[0].Text, "a error") {
		t.Fatalf("text = %#v", message)
	}
	if strings.Contains(message.Content[0].Text, "a hint") {
		t.Fatalf("a hint survived an error only report: %q", message.Content[0].Text)
	}
	// Only one file.
	injector.SetSeverities()
	injector.SetPaths("/project/b.go")
	message, _ = injector.Inject()
	if message == nil || !strings.Contains(message.Content[0].Text, "b warning") {
		t.Fatalf("text = %#v", message)
	}
	if strings.Contains(message.Content[0].Text, "a error") {
		t.Fatalf("another file survived a path scoped report: %q", message.Content[0].Text)
	}
	// A path with nothing wrong produces nothing rather than an empty region.
	injector.SetPaths("/project/clean.go")
	if message, _ = injector.Inject(); message != nil {
		t.Fatalf("an empty file produced a region: %q", message.Content[0].Text)
	}
	if _, err := NewDiagnosticsInjector(nil, 0); err == nil {
		t.Fatal("an injector without a pool was accepted")
	}
}

func TestInjectorKeepsAnUnrankedDiagnostic(t *testing.T) {
	pool := newTooledPool(t)
	injector, err := NewDiagnosticsInjector(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	// A diagnostic the server did not rank is not one to hide.
	pool.Client().Diagnostics().Replace(PathToURI("/a.go"), []Diagnostic{
		{Message: "unranked"},
	})
	injector.SetSeverities(SeverityError)
	message, _ := injector.Inject()
	if message == nil || !strings.Contains(message.Content[0].Text, "unranked") {
		t.Fatalf("message = %#v", message)
	}
}

func TestInjectorReportsAHookWiredToTheWrongThing(t *testing.T) {
	pool := newTooledPool(t)
	injector, err := NewDiagnosticsInjector(pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	pool.Client().Diagnostics().Replace(PathToURI("/a.go"), []Diagnostic{
		{Severity: SeverityError, Message: "broken"},
	})
	// The hook is given something that is not a request, which means the assembly
	// wired it somewhere it does not belong. Reporting that beats dropping the
	// diagnostics silently.
	out, err := injector.Hook()(context.Background(), "not a request")
	if err == nil {
		t.Fatal("the hook accepted the wrong type")
	}
	if out != "not a request" {
		t.Fatalf("the hook replaced the value: %v", out)
	}
	if !strings.Contains(err.Error(), "injector") {
		t.Fatalf("err = %v", err)
	}
}

func TestEscapeAttributeInTheInjector(t *testing.T) {
	escaped := EscapeAttribute(`a&b<c>d"e'f`)
	for _, wanted := range []string{"&amp;", "&lt;", "&gt;", "&quot;", "&#39;"} {
		if !strings.Contains(escaped, wanted) {
			t.Fatalf("escaped = %q", escaped)
		}
	}
}

func userMessage(text string) core.Message {
	return core.Message{
		Role:    core.RoleUser,
		Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: text}},
	}
}
