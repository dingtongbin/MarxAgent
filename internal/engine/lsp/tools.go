// SPDX-License-Identifier: Apache-2.0

package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// The tool names the design names. They are the only names the core loop sees.
const (
	ToolDefinition  = "lsp_definition"
	ToolReferences  = "lsp_references"
	ToolDiagnostics = "lsp_diagnostics"
	ToolHover       = "lsp_hover"
	ToolRename      = "lsp_rename"
)

// Pool owns the client and hands out tools.
//
// The tools are created up front rather than per call, because a tool's
// description is part of the cached prefix: a description that changed between
// turns would invalidate the cache on every turn.
type Pool struct {
	client *Client
	// open tracks the documents the server has been told about, with their versions.
	openMu sync.Mutex
	open   map[string]*Document
	// tools is built once.
	tools []core.Tool
}

// NewPool builds a pool over a client.
func NewPool(client *Client) (*Pool, error) {
	if client == nil {
		return nil, fmt.Errorf("lsp: a pool needs a client")
	}
	pool := &Pool{client: client, open: map[string]*Document{}}
	pool.tools = []core.Tool{
		&definitionTool{pool: pool},
		&referencesTool{pool: pool},
		&diagnosticsTool{pool: pool},
		&hoverTool{pool: pool},
		&renameTool{pool: pool},
	}
	return pool, nil
}

// Client exposes the underlying client.
func (p *Pool) Client() *Client { return p.client }

// Tools returns the tools, in a stable order so the prefix stays stable.
func (p *Pool) Tools() []core.Tool {
	out := make([]core.Tool, len(p.tools))
	copy(out, p.tools)
	return out
}

// Close shuts the server down.
func (p *Pool) Close() error { return p.client.Stop() }

// Open tells the server about a document, which every question needs: a server
// answers about a document it has been given.
func (p *Pool) Open(path, contents string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("lsp: cannot open a document with no path")
	}
	uri := PathToURI(path)
	if uri == "" {
		return fmt.Errorf("lsp: cannot open a document with no path")
	}
	p.openMu.Lock()
	document, existed := p.open[uri]
	if existed {
		document.Version++
		document.OpenedAt = time.Now().UTC()
	} else {
		document = &Document{
			URI:        uri,
			Version:    FirstDocumentVersion,
			LanguageID: LanguageFor(path),
			OpenedAt:   time.Now().UTC(),
		}
		p.open[uri] = document
	}
	version := document.Version
	language := document.LanguageID
	p.openMu.Unlock()

	if err := p.client.Ensure(context.Background()); err != nil {
		// The document is remembered, so a later call does not have to read the file
		// again; only the notification was lost.
		return err
	}
	if existed {
		return p.client.notify(MethodDidChange, mustMarshal(map[string]any{
			"textDocument":   VersionedTextDocumentIdentifier{URI: uri, Version: version},
			"contentChanges": []map[string]any{{"text": contents}},
		}))
	}
	return p.client.notify(MethodDidOpen, mustMarshal(map[string]any{
		"textDocument": TextDocumentItem{
			URI: uri, LanguageID: language, Version: version, Text: contents,
		},
	}))
}

// CloseDocument tells the server a document is gone.
func (p *Pool) CloseDocument(path string) error {
	uri := PathToURI(path)
	p.openMu.Lock()
	delete(p.open, uri)
	p.openMu.Unlock()
	if !p.client.Running() {
		return nil
	}
	return p.client.notify(MethodDidClose, mustMarshal(map[string]any{
		"textDocument": TextDocumentIdentifier{URI: uri},
	}))
}

// Opened reports which documents the server has been told about, sorted.
func (p *Pool) Opened() []string {
	p.openMu.Lock()
	defer p.openMu.Unlock()
	uris := make([]string, 0, len(p.open))
	for uri := range p.open {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	return uris
}

// documentVersion returns the version the server last saw for a document.
func (p *Pool) documentVersion(uri string) int {
	p.openMu.Lock()
	defer p.openMu.Unlock()
	if document, known := p.open[uri]; known {
		return document.Version
	}
	return 0
}

// jsonResult builds a tool result whose output is the text a model reads, with the
// counts a caller acts on carried alongside it.
//
// The output is a json object rather than a bare string because the core model
// carries a tool result as raw json, and a bare string would leave every caller
// guessing the shape.
func jsonResult(text string, metadata map[string]any) core.ToolResult {
	payload := map[string]any{"text": text}
	for key, value := range metadata {
		payload[key] = value
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Every value here is a string or a count, so this is unreachable; an empty
		// object is the least harmful thing to return rather than a panic.
		return core.ToolResult{Output: json.RawMessage("{}")}
	}
	return core.ToolResult{Output: encoded}
}

func mustMarshal(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		// Every value marshalled here is built from plain types, so this is a
		// programming error rather than a runtime condition, and an empty object is
		// the least harmful thing to send.
		return []byte("{}")
	}
	return encoded
}

// positionParams is the shape every position based tool takes.
type positionParams struct {
	// Path is the file, absolute.
	Path string `json:"path"`
	// Line is one based, because that is what a user reads in an editor and what a
	// model produces when quoting a diagnostic.
	Line int `json:"line"`
	// Column is one based.
	Column int `json:"column"`
	// Contents is the document's text, when the caller already has it. Supplying
	// it saves a read and a notification.
	Contents string `json:"contents,omitempty"`
}

// resolve turns the tool's parameters into protocol parameters, telling the server
// about the document when the caller did not.
func (p *Pool) resolve(params positionParams) (TextDocumentPositionParams, error) {
	if strings.TrimSpace(params.Path) == "" {
		return TextDocumentPositionParams{}, fmt.Errorf("lsp: a path is required")
	}
	if params.Line < 1 || params.Column < 1 {
		// The tool takes one based coordinates because that is what a diagnostic
		// quotes, and the protocol wants zero based ones. Rejecting a zero rather
		// than subtracting from it is what makes an off by one loud instead of
		// pointing at the wrong line.
		return TextDocumentPositionParams{}, fmt.Errorf(
			"lsp: line and column are one based, got line %d column %d", params.Line, params.Column)
	}
	uri := PathToURI(params.Path)
	if params.Contents != "" {
		if err := p.Open(params.Path, params.Contents); err != nil {
			return TextDocumentPositionParams{}, err
		}
	} else if p.documentVersion(uri) == 0 {
		return TextDocumentPositionParams{}, fmt.Errorf(
			"lsp: %q has not been opened, so supply its contents with the request", params.Path)
	}
	return TextDocumentPositionParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
		Position: Position{
			Line:      params.Line - 1,
			Character: params.Column - 1,
		},
	}, nil
}

// baseTool carries what every tool in the pool needs.
type baseTool struct {
	pool *Pool
	// name and description are fixed English, because they are part of the cached
	// prefix and a change to either is a cache invalidation for every session.
	name        string
	description string
	schema      string
}

// Name identifies the tool.
func (t *baseTool) Name() string { return t.name }

// Description explains the tool, in the language the model reads best.
func (t *baseTool) Description() string { return t.description }

// Parameters is the tool's json schema.
func (t *baseTool) Parameters() json.RawMessage { return json.RawMessage(t.schema) }

func (t *baseTool) execute(ctx context.Context, params positionParams) (any, error) {
	resolved, err := t.pool.resolve(params)
	if err != nil {
		return nil, err
	}
	return t.ask(ctx, resolved)
}

func (t *baseTool) ask(context.Context, TextDocumentPositionParams) (any, error) {
	return nil, fmt.Errorf("lsp: not implemented")
}

func decodeParams(raw json.RawMessage) (positionParams, error) {
	var params positionParams
	if len(raw) == 0 {
		return params, fmt.Errorf("lsp: the call had no parameters")
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return params, fmt.Errorf("lsp: the parameters are not valid: %w", err)
	}
	return params, nil
}

// definitionTool answers where a symbol is defined.
type definitionTool struct{ baseTool }

// Name identifies the tool.
func (t *definitionTool) Name() string { return ToolDefinition }

// Description explains the tool.
func (t *definitionTool) Description() string {
	return "Find where the symbol at a position is defined, by starting the language " +
		"server for the language of the file and asking it. Use this before reading a " +
		"file you have only heard the name of, and instead of grepping when the symbol " +
		"belongs to another package."
}

// Parameters is the tool's json schema.
func (t *definitionTool) Parameters() json.RawMessage {
	return json.RawMessage(positionSchema)
}

// Execute asks the server.
func (t *definitionTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	params, err := decodeParams(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	resolved, err := t.pool.resolve(params)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !t.pool.client.Capabilities().Supports(MethodTextDocumentDefinition) {
		return core.ToolResult{}, fmt.Errorf(
			"lsp: the language server for this file does not provide definitions")
	}
	result, err := t.pool.client.Request(ctx, MethodTextDocumentDefinition, resolved)
	if err != nil {
		return core.ToolResult{}, err
	}
	locations, err := decodeLocations(result)
	if err != nil {
		return core.ToolResult{}, err
	}
	return jsonResult(formatLocations("definitions", locations),
		map[string]any{"count": len(locations)}), nil
}

// referencesTool answers where a symbol is used.
type referencesTool struct{ baseTool }

// Name identifies the tool.
func (t *referencesTool) Name() string { return ToolReferences }

// Description explains the tool.
func (t *referencesTool) Description() string {
	return "Find every place the symbol at a position is used, by asking the language " +
		"server. Use this before renaming anything, so the rename does not miss a " +
		"caller, and instead of grepping when the symbol is a method or a field."
}

// Parameters is the tool's json schema.
func (t *referencesTool) Parameters() json.RawMessage {
	return json.RawMessage(referencesSchema)
}

// Execute asks the server.
func (t *referencesTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	params, err := decodeParams(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	resolved, err := t.pool.resolve(params)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !t.pool.client.Capabilities().Supports(MethodTextDocumentReferences) {
		return core.ToolResult{}, fmt.Errorf(
			"lsp: the language server for this file does not provide references")
	}
	request := ReferenceParams{TextDocumentPositionParams: resolved}
	request.Context.IncludeDeclaration = true
	result, err := t.pool.client.Request(ctx, MethodTextDocumentReferences, request)
	if err != nil {
		return core.ToolResult{}, err
	}
	locations, err := decodeLocations(result)
	if err != nil {
		return core.ToolResult{}, err
	}
	// The count is reported separately because it is the number a caller decides
	// on, and a list that has to be counted by eye is a list nobody counts.
	byFile := groupByFile(locations)
	return jsonResult(formatLocations("references", locations),
		map[string]any{"count": len(locations), "files": len(byFile)}), nil
}

// hoverTool answers what a symbol is.
type hoverTool struct{ baseTool }

// Name identifies the tool.
func (t *hoverTool) Name() string { return ToolHover }

// Description explains the tool.
func (t *hoverTool) Description() string {
	return "Describe the symbol at a position: its type, its documentation and its " +
		"signature, as the language server reports them. Use this when you know a " +
		"name and need to know what it is without reading its whole definition."
}

// Parameters is the tool's json schema.
func (t *hoverTool) Parameters() json.RawMessage {
	return json.RawMessage(positionSchema)
}

// Execute asks the server.
func (t *hoverTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	params, err := decodeParams(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	resolved, err := t.pool.resolve(params)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !t.pool.client.Capabilities().Supports(MethodTextDocumentHover) {
		return core.ToolResult{}, fmt.Errorf(
			"lsp: the language server for this file does not provide hovers")
	}
	result, err := t.pool.client.Request(ctx, MethodTextDocumentHover, resolved)
	if err != nil {
		return core.ToolResult{}, err
	}
	if len(result) == 0 || string(result) == "null" {
		return jsonResult("The language server has nothing to say about that position.", nil), nil
	}
	var hover Hover
	if err := json.Unmarshal(result, &hover); err != nil {
		return core.ToolResult{}, fmt.Errorf("lsp: decode the hover: %w", err)
	}
	if strings.TrimSpace(hover.Contents.Value) == "" {
		return jsonResult("The language server has nothing to say about that position.", nil), nil
	}
	return jsonResult(hover.Contents.Value, nil), nil
}

// renameTool answers what a rename would change.
type renameTool struct{ baseTool }

// Name identifies the tool.
func (t *renameTool) Name() string { return ToolRename }

// Description explains the tool.
func (t *renameTool) Description() string {
	return "Produce the edits a rename would make, without applying them. The language " +
		"server decides every place that has to change, so a rename cannot miss a " +
		"caller. Apply the returned edits yourself; this tool never writes a file."
}

// Parameters is the tool's json schema.
func (t *renameTool) Parameters() json.RawMessage {
	return json.RawMessage(renameSchema)
}

// Execute asks the server.
func (t *renameTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	params, err := decodeParams(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	renaming, err := decodeRename(raw)
	if err != nil {
		return core.ToolResult{}, err
	}
	if strings.TrimSpace(renaming.NewName) == "" {
		return core.ToolResult{}, fmt.Errorf("lsp: a rename needs a new name")
	}
	resolved, err := t.pool.resolve(params)
	if err != nil {
		return core.ToolResult{}, err
	}
	if !t.pool.client.Capabilities().Supports(MethodTextDocumentRename) {
		return core.ToolResult{}, fmt.Errorf(
			"lsp: the language server for this file does not provide renames")
	}
	result, err := t.pool.client.Request(ctx, MethodTextDocumentRename, RenameParams{
		TextDocumentPositionParams: resolved,
		NewName:                    renaming.NewName,
	})
	if err != nil {
		return core.ToolResult{}, err
	}
	var edit WorkspaceEdit
	if len(result) > 0 {
		if err := json.Unmarshal(result, &edit); err != nil {
			return core.ToolResult{}, fmt.Errorf("lsp: decode the rename: %w", err)
		}
	}
	// The edits are returned as json, because a model applies them with the write
	// tools and a prose description of a range is one it would get wrong. The
	// applied flag says plainly that nothing was written: this tool produces an
	// edit set and never touches a file.
	editCount := 0
	for _, edits := range edit.Changes {
		editCount += len(edits)
	}
	encoded, err := json.Marshal(edit)
	if err != nil {
		return core.ToolResult{}, fmt.Errorf("lsp: encode the rename: %w", err)
	}
	return core.ToolResult{
		Output: mustMarshal(map[string]any{
			"text":     string(encoded),
			"files":    len(edit.Changes),
			"edits":    editCount,
			"applied":  false,
			"new_name": renaming.NewName,
		}),
	}, nil
}

// diagnosticsTool reports what the server has found.
type diagnosticsTool struct{ baseTool }

// Name identifies the tool.
func (t *diagnosticsTool) Name() string { return ToolDiagnostics }

// Description explains the tool.
func (t *diagnosticsTool) Description() string {
	return "Report the problems the language server has found in a file, or in every " +
		"open file when no path is given. Use this after editing rather than reading a " +
		"file back to find out whether it compiles."
}

// Parameters is the tool's json schema.
func (t *diagnosticsTool) Parameters() json.RawMessage {
	return json.RawMessage(diagnosticsSchema)
}

// Execute reports the stored diagnostics.
//
// The diagnostics are read from the store rather than requested, because a server
// pushes them as it works and a request would either wait for the next push or
// block until a timeout. A caller that wants them fresh opens the document first.
func (t *diagnosticsTool) Execute(_ context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var params struct {
		Path string `json:"path,omitempty"`
		// Severity filters the report to the worst level the caller cares about.
		Severity string `json:"severity,omitempty"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return core.ToolResult{}, fmt.Errorf("lsp: the parameters are not valid: %w", err)
		}
	}
	store := t.pool.client.Diagnostics()
	var lines []string
	total := 0
	if strings.TrimSpace(params.Path) != "" {
		uri := PathToURI(params.Path)
		lines = append(lines, formatDiagnostics(params.Path, store.For(uri))...)
		total = store.Count(uri)
	} else {
		for _, uri := range store.URIs() {
			path, err := URIToPath(uri)
			if err != nil {
				path = uri
			}
			lines = append(lines, formatDiagnostics(path, store.For(uri))...)
			total += store.Count(uri)
		}
	}
	filtered, dropped := filterBySeverity(lines, params.Severity)
	if len(filtered) == 0 {
		message := "The language server has reported no problems."
		if dropped > 0 {
			message = fmt.Sprintf(
				"The language server reported %d problems, none at or above %q.", dropped, params.Severity)
		}
		return jsonResult(message, map[string]any{"count": 0}), nil
	}
	header := fmt.Sprintf("%d problems reported by the language server:", total)
	return jsonResult(header+"\n"+strings.Join(filtered, "\n"),
		map[string]any{"count": total, "reported": len(filtered)}), nil
}

// formatDiagnostics renders one file's diagnostics as lines a model can read.
//
// The coordinates are rendered one based because that is how a diagnostic is
// quoted everywhere else, and a report that used a different base from the tool
// that produced the position would be a trap.
func formatDiagnostics(path string, diagnostics []Diagnostic) []string {
	lines := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		lines = append(lines, fmt.Sprintf("%s:%d:%d: %s: %s",
			path,
			diagnostic.Range.Start.Line+1,
			diagnostic.Range.Start.Character+1,
			diagnostic.Severity,
			diagnostic.Message))
	}
	return lines
}

// filterBySeverity keeps the lines at or above a named severity. A line is parsed
// back out of its rendered form, which is not elegant; the alternative is to carry
// the severity alongside, and a report that says which lines it dropped is worth
// more than the elegance.
func filterBySeverity(lines []string, severity string) ([]string, int) {
	severity = strings.ToLower(strings.TrimSpace(severity))
	if severity == "" {
		return lines, 0
	}
	wanted, known := severityRank(severity)
	if !known {
		// An unknown severity filters nothing rather than everything, because
		// silently hiding every problem is the worse failure.
		return lines, 0
	}
	kept := make([]string, 0, len(lines))
	dropped := 0
	for _, line := range lines {
		if rankOfLine(line) <= wanted {
			kept = append(kept, line)
			continue
		}
		dropped++
	}
	return kept, dropped
}

func severityRank(name string) (DiagnosticSeverity, bool) {
	switch name {
	case "error":
		return SeverityError, true
	case "warning", "warn":
		return SeverityWarning, true
	case "information", "info":
		return SeverityInformation, true
	case "hint":
		return SeverityHint, true
	default:
		return 0, false
	}
}

func rankOfLine(line string) DiagnosticSeverity {
	for _, candidate := range []DiagnosticSeverity{
		SeverityError, SeverityWarning, SeverityInformation, SeverityHint,
	} {
		if strings.Contains(line, ": "+candidate.String()+": ") {
			return candidate
		}
	}
	// A line with no severity is treated as the worst, because a diagnostic the
	// server did not rank is not a diagnostic to hide.
	return SeverityError
}

// decodeLocations parses a definition or reference answer, which the protocol
// allows to be one location or a list of them.
func decodeLocations(result json.RawMessage) ([]Location, error) {
	if len(result) == 0 || string(result) == "null" {
		return nil, nil
	}
	var many []Location
	if err := json.Unmarshal(result, &many); err == nil {
		return many, nil
	}
	var one Location
	if err := json.Unmarshal(result, &one); err != nil {
		return nil, fmt.Errorf("lsp: decode the locations: %w", err)
	}
	return []Location{one}, nil
}

// formatLocations renders locations, sorted so two runs over the same answer agree.
func formatLocations(kind string, locations []Location) string {
	if len(locations) == 0 {
		return fmt.Sprintf("The language server found no %s.", kind)
	}
	paths := make([]string, 0, len(locations))
	for _, location := range locations {
		path, err := URIToPath(location.URI)
		if err != nil {
			path = location.URI
		}
		paths = append(paths, fmt.Sprintf("%s:%d:%d",
			path, location.Range.Start.Line+1, location.Range.Start.Character+1))
	}
	sort.Strings(paths)
	var builder strings.Builder
	fmt.Fprintf(&builder, "%d %s:", len(paths), kind)
	for _, path := range paths {
		builder.WriteString("\n")
		builder.WriteString(path)
	}
	return builder.String()
}

// groupByFile counts how many files a set of locations touches, which is what a
// caller needs to decide whether a rename is small.
func groupByFile(locations []Location) map[string]int {
	grouped := make(map[string]int, len(locations))
	for _, location := range locations {
		grouped[location.URI]++
	}
	return grouped
}

func decodeRename(raw json.RawMessage) (struct {
	NewName string `json:"new_name"`
}, error) {
	var renaming struct {
		NewName string `json:"new_name"`
	}
	if len(raw) == 0 {
		return renaming, fmt.Errorf("lsp: the call had no parameters")
	}
	if err := json.Unmarshal(raw, &renaming); err != nil {
		return renaming, fmt.Errorf("lsp: the parameters are not valid: %w", err)
	}
	return renaming, nil
}

const positionSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute path to the file."},
    "line": {"type": "integer", "description": "One based line number."},
    "column": {"type": "integer", "description": "One based column number."},
    "contents": {"type": "string", "description": "The file contents, when already known."}
  },
  "required": ["path", "line", "column"],
  "additionalProperties": false
}`

const referencesSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute path to the file."},
    "line": {"type": "integer", "description": "One based line number."},
    "column": {"type": "integer", "description": "One based column number."},
    "contents": {"type": "string", "description": "The file contents, when already known."}
  },
  "required": ["path", "line", "column"],
  "additionalProperties": false
}`

const renameSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute path to the file."},
    "line": {"type": "integer", "description": "One based line number."},
    "column": {"type": "integer", "description": "One based column number."},
    "new_name": {"type": "string", "description": "The new name for the symbol."},
    "contents": {"type": "string", "description": "The file contents, when already known."}
  },
  "required": ["path", "line", "column", "new_name"],
  "additionalProperties": false
}`

const diagnosticsSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Absolute path to one file, or omit for every open file."},
    "severity": {
      "type": "string",
      "enum": ["error", "warning", "information", "hint"],
      "description": "Report only problems at or above this severity."
    }
  },
  "additionalProperties": false
}`
