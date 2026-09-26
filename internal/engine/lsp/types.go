// SPDX-License-Identifier: Apache-2.0

package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The protocol method names this client uses.
const (
	MethodInitialize             = "initialize"
	MethodInitialized            = "initialized"
	MethodShutdown               = "shutdown"
	MethodExit                   = "exit"
	MethodTextDocumentHover      = "textDocument/hover"
	MethodTextDocumentDefinition = "textDocument/definition"
	MethodTextDocumentReferences = "textDocument/references"
	MethodTextDocumentRename     = "textDocument/rename"
	MethodPublishDiagnostics     = "textDocument/publishDiagnostics"
	MethodDidOpen                = "textDocument/didOpen"
	MethodDidChange              = "textDocument/didChange"
	MethodDidClose               = "textDocument/didClose"
	MethodDidSave                = "textDocument/didSave"
)

// Document versions start at one, because zero is what a document that was never
// opened carries.
const FirstDocumentVersion = 1

// PushCapability asks a server to push rather than wait to be asked.
var PushCapability = DynamicRegistrationCapability{DynamicRegistration: true}

// InitializeParams is the handshake request.
type InitializeParams struct {
	ProcessID    int                `json:"processId"`
	RootURI      string             `json:"rootUri,omitempty"`
	RootPath     string             `json:"rootPath,omitempty"`
	Capabilities ClientCapabilities `json:"capabilities"`
	Trace        string             `json:"trace,omitempty"`
}

// ClientCapabilities is what this client can do.
type ClientCapabilities struct {
	TextDocument TextDocumentClientCapabilities `json:"textDocument"`
	Workspace    WorkspaceClientCapabilities    `json:"workspace"`
}

// TextDocumentClientCapabilities is the document half.
type TextDocumentClientCapabilities struct {
	Synchronization    DynamicRegistrationCapability        `json:"synchronization"`
	PublishDiagnostics PublishDiagnosticsClientCapabilities `json:"publishDiagnostics"`
	Hover              DynamicRegistrationCapability        `json:"hover"`
	Definition         DynamicRegistrationCapability        `json:"definition"`
	References         DynamicRegistrationCapability        `json:"references"`
	Rename             DynamicRegistrationCapability        `json:"rename"`
}

// DynamicRegistrationCapability asks a server to push.
type DynamicRegistrationCapability struct {
	DynamicRegistration bool `json:"dynamicRegistration"`
}

// PublishDiagnosticsClientCapabilities says whether related information is wanted.
type PublishDiagnosticsClientCapabilities struct {
	RelatedInformation bool `json:"relatedInformation"`
}

// WorkspaceClientCapabilities is the workspace half.
type WorkspaceClientCapabilities struct {
	WorkspaceFolders bool `json:"workspaceFolders"`
}

// ServerCapabilities is what a server said it supports. Only the parts that change
// what this client does are carried, because a client that models the whole
// protocol would model a protocol version it does not speak.
type ServerCapabilities struct {
	// HoverProvider, DefinitionProvider, ReferencesProvider and RenameProvider are
	// tri state in the protocol: absent, false or a registration option. Only a
	// true value enables the tool.
	HoverProvider      *bool `json:"hoverProvider,omitempty"`
	DefinitionProvider *bool `json:"definitionProvider,omitempty"`
	ReferencesProvider *bool `json:"referencesProvider,omitempty"`
	RenameProvider     *bool `json:"renameProvider,omitempty"`
	DiagnosticProvider *bool `json:"diagnosticProvider,omitempty"`
	TextDocumentSync   *int  `json:"textDocumentSync,omitempty"`
	Workspace          struct {
		WorkspaceFolders struct {
			Supported           bool `json:"supported"`
			ChangeNotifications bool `json:"changeNotifications"`
		} `json:"workspaceFolders"`
	} `json:"workspace"`
}

// Supports reports whether a capability is on. A capability the server did not
// mention is off, because sending a request for it would only earn an error.
func (c ServerCapabilities) Supports(capability string) bool {
	switch capability {
	case MethodTextDocumentHover:
		return isEnabled(c.HoverProvider)
	case MethodTextDocumentDefinition:
		return isEnabled(c.DefinitionProvider)
	case MethodTextDocumentReferences:
		return isEnabled(c.ReferencesProvider)
	case MethodTextDocumentRename:
		return isEnabled(c.RenameProvider)
	default:
		return false
	}
}

func isEnabled(value *bool) bool { return value != nil && *value }

// Position is a zero based line and character offset.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Before reports whether this position comes first.
func (p Position) Before(other Position) bool {
	if p.Line != other.Line {
		return p.Line < other.Line
	}
	return p.Character < other.Character
}

// Range is a span in a document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range in a document.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// TextDocumentIdentifier names a document.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// VersionedTextDocumentIdentifier names a document at a version.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

// TextDocumentItem is a document's contents.
type TextDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// TextDocumentPositionParams is a question about a place in a document.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// ReferenceParams is a question about the places something is used.
type ReferenceParams struct {
	TextDocumentPositionParams
	// Context says whether the declaration itself counts as a reference.
	Context struct {
		IncludeDeclaration bool `json:"includeDeclaration"`
	} `json:"context"`
}

// RenameParams is a request to rename something.
type RenameParams struct {
	TextDocumentPositionParams
	NewName string `json:"newName"`
}

// Hover is what a server answers about a symbol.
type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// MarkupContent is formatted text.
type MarkupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// The markup kinds the protocol defines. A client that does not know the kind
// still shows the value, so an unknown one is not an error.
const (
	// MarkupPlainText is unformatted.
	MarkupPlainText = "plaintext"
	// MarkupMarkdown is markdown.
	MarkupMarkdown = "markdown"
)

// WorkspaceEdit is what a rename produces.
type WorkspaceEdit struct {
	// Changes maps a document URI to the edits to make in it.
	Changes map[string][]TextEdit `json:"changes,omitempty"`
}

// TextEdit replaces a range with new text.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// DiagnosticSeverity ranks a diagnostic.
type DiagnosticSeverity int

// The severities the protocol defines.
const (
	// SeverityError means the code is wrong.
	SeverityError DiagnosticSeverity = 1
	// SeverityWarning means the code is suspicious.
	SeverityWarning DiagnosticSeverity = 2
	// SeverityInformation is advice.
	SeverityInformation DiagnosticSeverity = 3
	// SeverityHint is a suggestion.
	SeverityHint DiagnosticSeverity = 4
)

// String names a severity, so a diagnostic is readable without the table.
func (s DiagnosticSeverity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	case SeverityInformation:
		return "information"
	case SeverityHint:
		return "hint"
	default:
		return "unknown"
	}
}

// Diagnostic is one problem the server found.
type Diagnostic struct {
	Range    Range              `json:"range"`
	Severity DiagnosticSeverity `json:"severity,omitempty"`
	Code     json.RawMessage    `json:"code,omitempty"`
	Source   string             `json:"source,omitempty"`
	Message  string             `json:"message"`
}

// DiagnosticsParams is a push of one document's diagnostics.
type DiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// DiagnosticStore holds the diagnostics a server has published.
//
// The protocol pushes a full list per document rather than a change, so the store
// replaces rather than merges. Replacing is also what makes a cleared document
// come out empty instead of keeping whatever it had before.
type DiagnosticStore struct {
	mu      sync.RWMutex
	byURI   map[string][]Diagnostic
	version map[string]int
}

// NewDiagnosticStore builds an empty store.
func NewDiagnosticStore() *DiagnosticStore {
	return &DiagnosticStore{
		byURI:   map[string][]Diagnostic{},
		version: map[string]int{},
	}
}

// Publish replaces one document's diagnostics.
func (s *DiagnosticStore) Publish(params json.RawMessage) error {
	var decoded DiagnosticsParams
	if err := json.Unmarshal(params, &decoded); err != nil {
		return fmt.Errorf("lsp: decode the diagnostics: %w", err)
	}
	s.Replace(decoded.URI, decoded.Diagnostics)
	return nil
}

// Replace sets one document's diagnostics.
func (s *DiagnosticStore) Replace(uri string, diagnostics []Diagnostic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(diagnostics) == 0 {
		delete(s.byURI, uri)
		return
	}
	copied := make([]Diagnostic, len(diagnostics))
	copy(copied, diagnostics)
	s.byURI[uri] = copied
}

// Forget drops one document's diagnostics, which is what closing a document means.
func (s *DiagnosticStore) Forget(uri string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byURI, uri)
}

// For returns one document's diagnostics, in the order the server sent them.
func (s *DiagnosticStore) For(uri string) []Diagnostic {
	s.mu.RLock()
	defer s.mu.RUnlock()
	diagnostics := s.byURI[uri]
	out := make([]Diagnostic, len(diagnostics))
	copy(out, diagnostics)
	return out
}

// Count returns how many diagnostics a document has.
func (s *DiagnosticStore) Count(uri string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byURI[uri])
}

// Total returns how many diagnostics are held across every document.
func (s *DiagnosticStore) Total() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
	for _, diagnostics := range s.byURI {
		total += len(diagnostics)
	}
	return total
}

// URIs returns the documents with diagnostics, sorted so a report is stable.
func (s *DiagnosticStore) URIs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uris := make([]string, 0, len(s.byURI))
	for uri := range s.byURI {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	return uris
}

// Errors returns only the diagnostics that are errors, which is what a turn needs
// to be told about a build that will not pass.
func (s *DiagnosticStore) Errors() []Diagnostic {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Diagnostic
	for _, diagnostics := range s.byURI {
		for _, diagnostic := range diagnostics {
			if diagnostic.Severity == SeverityError {
				out = append(out, diagnostic)
			}
		}
	}
	return out
}

// Snapshot returns every diagnostic, keyed by document and in a stable order.
func (s *DiagnosticStore) Snapshot() map[string][]Diagnostic {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]Diagnostic, len(s.byURI))
	for uri, diagnostics := range s.byURI {
		copied := make([]Diagnostic, len(diagnostics))
		copy(copied, diagnostics)
		out[uri] = copied
	}
	return out
}

// Clear empties the store.
func (s *DiagnosticStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byURI = map[string][]Diagnostic{}
}

// PathToURI converts a filesystem path into the URI the protocol uses.
//
// The conversion has to be right on all three platforms: a URI built by joining
// with a forward slash is wrong on Windows, and a space that is not escaped makes
// the server fail to open the file at all.
func PathToURI(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "file://") {
		return path
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		if resolved, err := filepath.Abs(absolute); err == nil {
			absolute = resolved
		}
	}
	// The slash form already carries a Windows drive letter, and the protocol
	// writes that letter after the empty authority: file:///C:/dir/file. Adding the
	// volume separately would produce file:///C:C:/dir/file, which names nothing.
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	// The path is escaped by hand rather than through url.URL, because url.URL
	// re-encodes a colon that the authority form needs to keep as a separator.
	escaped := (&url.URL{Path: slashed}).EscapedPath()
	return "file://" + escaped
}

// URIToPath converts a URI back into a filesystem path, and reports a URI it
// cannot handle rather than returning something that looks like a path.
func URIToPath(uri string) (string, error) {
	if uri == "" {
		return "", fmt.Errorf("lsp: the uri is empty")
	}
	if !strings.HasPrefix(uri, "file:") {
		return "", fmt.Errorf("lsp: %q is not a file uri", uri)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("lsp: parse %q: %w", uri, err)
	}
	path := parsed.Path
	if parsed.Host != "" {
		// A file URI with a host is a network share, which is a real path on Windows
		// and nowhere else, so it is passed through with the host restored rather
		// than silently producing a path that does not exist.
		return parsed.Host + path, nil
	}
	fromSlash := filepath.FromSlash(path)
	// A Windows drive letter is written after the authority in a file uri, so the
	// path arrives as /D:/dir/file. Left alone that becomes a rooted path with a
	// stray separator, which names a directory that does not exist.
	if volume := filepath.VolumeName(fromSlash); volume == "" && len(fromSlash) >= 3 &&
		fromSlash[0] == filepath.Separator && fromSlash[2] == ':' {
		return fromSlash[1:], nil
	}
	return fromSlash, nil
}

// LanguageFor maps a file extension onto the language identifier the protocol
// expects. An unknown extension gets plaintext, which every server accepts, so an
// unfamiliar file is still openable rather than refused.
func LanguageFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".c", ".h":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp", ".hh":
		return "cpp"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	case ".cs":
		return "csharp"
	case ".kt", ".kts":
		return "kotlin"
	case ".swift":
		return "swift"
	case ".php":
		return "php"
	case ".md", ".markdown":
		return "markdown"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".sh", ".bash":
		return "shellscript"
	default:
		return "plaintext"
	}
}

// Document is a file the server has been told about.
type Document struct {
	// URI is the document's uri.
	URI string
	// Version increments on every change, because the protocol uses it to reject an
	// answer about a version that no longer exists.
	Version int
	// LanguageID is what the server was told the document is.
	LanguageID string
	// OpenedAt is when the server was told.
	OpenedAt time.Time
}
