// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// TemplateFileName is the file a template lives in, inside a directory named for
// the template.
//
// A template is a file rather than a setting because a sub agent's instructions and
// its tool set are the two things most likely to need changing by a person who is
// not this project's author. A setting would put them behind a rebuild; a file lets
// a project add a sub agent, or narrow one, without touching the binary.
const TemplateFileName = "TEMPLATE.md"

const (
	defaultMaxTemplateBytes   int64 = 256 << 10
	defaultMaxTemplateResults       = 32
	// maxTemplateTools bounds a template's tool set, because a list nobody can hold
	// in their head is a list nobody chose.
	maxTemplateTools = 32
)

var (
	// ErrNilContext is a request made without a context. It is declared here rather
	// than borrowed from another package because a missing context is a mistake in
	// the caller's own code and the message should name the package it happened in.
	ErrNilContext = errors.New("subagent: context must not be nil")
	// ErrTemplateNotFound is a request for a template that is not on disk.
	ErrTemplateNotFound = errors.New("subagent: template not found")
	// ErrInvalidTemplate is a template that cannot be used as written.
	ErrInvalidTemplate = errors.New("subagent: invalid template")
)

// TemplateSource is where a template came from. A project template shadows a global
// one of the same name, which is how a repository narrows a sub agent its users
// inherit without asking anyone to uninstall anything.
type TemplateSource string

const (
	SourceGlobal  TemplateSource = "global"
	SourceProject TemplateSource = "project"
)

// TemplateConfig is where templates are looked for.
type TemplateConfig struct {
	// GlobalDir holds templates the user installed.
	GlobalDir string
	// ProjectDir holds templates the project ships. It shadows GlobalDir by name.
	ProjectDir string
	// MaxFileBytes bounds one template file. Zero selects the default.
	MaxFileBytes int64
	// MaxResults bounds a search. Zero selects the default.
	MaxResults int
}

// TemplateEntry is the cheap half of a template: what it is called, what it is for,
// and what it is allowed to use.
//
// The entry is what a caller sees before deciding, so it stays small enough to put
// in front of a model without thinking about it. Instructions are not in it.
type TemplateEntry struct {
	// Name is the identifier, which is also the directory name.
	Name string `json:"name"`
	// Description is one sentence on what this sub agent is for, which is what the
	// main core reads to decide whether this is the sub agent it wants.
	Description string `json:"description"`
	// Keywords are extra search terms, for a main core that knows what it wants but
	// not what the template is called.
	Keywords []string `json:"keywords,omitempty"`
	// Tools are the names this sub agent may use. It is a list rather than a
	// pattern because a sub agent is confined to what the main core hands it, and a
	// pattern would let a sub agent reach something the main core never chose.
	Tools []string `json:"tools,omitempty"`
	// Model is the model to run the sub agent on, empty for the main core's own.
	// A sub agent that has to think about something expensive is a decision somebody
	// should make once, in a file, rather than every time a sub agent is created.
	Model string `json:"model,omitempty"`
	// MaxIterations bounds the sub agent's own loop, because a sub agent that keeps
	// thinking is a sub agent that spends the main core's budget without returning.
	MaxIterations int `json:"max_iterations,omitempty"`
	// Source is where it was found.
	Source TemplateSource `json:"source"`
	// Root and File are where it lives, used to read it again.
	Root string `json:"-"`
	File string `json:"-"`
}

// Template is a loaded template: the entry plus the instructions the sub agent runs
// on.
type Template struct {
	TemplateEntry
	// Instructions are the sub agent's own prompt. It is separate from the main
	// core's on purpose: a sub agent running on its parent's prompt would inherit
	// permissions nobody meant to hand it.
	Instructions string `json:"instructions"`
}

// templateFrontmatter is the header of a template file.
type templateFrontmatter struct {
	Name          string   `yaml:"name"`
	Description   string   `yaml:"description"`
	Keywords      []string `yaml:"keywords"`
	Tools         []string `yaml:"tools"`
	Model         string   `yaml:"model"`
	MaxIterations int      `yaml:"max_iterations"`
}

// TemplatePool holds the templates on disk.
//
// The shape is the same one the skills pool uses, on purpose: a cheap index for
// deciding and a full read for using. Two pools that load the same way are one
// thing to learn rather than two.
type TemplatePool struct {
	globalDir  string
	projectDir string
	maxBytes   int64
	maxResults int

	refreshMu sync.Mutex
	mu        sync.RWMutex
	entries   map[string]TemplateEntry
	order     []string
}

// NewTemplatePool builds a pool. Neither directory has to exist, because a machine
// with no sub agents installed is a normal machine and not a misconfigured one.
func NewTemplatePool(config TemplateConfig) (*TemplatePool, error) {
	globalDir, err := normalizeTemplateRoot(config.GlobalDir)
	if err != nil {
		return nil, fmt.Errorf("subagent: global template directory: %w", err)
	}
	projectDir, err := normalizeTemplateRoot(config.ProjectDir)
	if err != nil {
		return nil, fmt.Errorf("subagent: project template directory: %w", err)
	}
	maxBytes := config.MaxFileBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxTemplateBytes
	}
	if maxBytes < 1 {
		return nil, fmt.Errorf("subagent: max template bytes must be positive")
	}
	maxResults := config.MaxResults
	if maxResults == 0 {
		maxResults = defaultMaxTemplateResults
	}
	if maxResults < 1 {
		return nil, fmt.Errorf("subagent: max results must be positive")
	}
	return &TemplatePool{
		globalDir:  globalDir,
		projectDir: projectDir,
		maxBytes:   maxBytes,
		maxResults: maxResults,
		entries:    make(map[string]TemplateEntry),
	}, nil
}

// Refresh rescans both directories.
//
// It keeps the order it had, so a rescanned template does not move in the index the
// main core has already read. Order is a cache: a template that jumps around between
// turns invalidates the prefix in front of the model for no reason.
func (p *TemplatePool) Refresh(ctx context.Context) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.mu.RLock()
	previous := append([]string(nil), p.order...)
	p.mu.RUnlock()

	entries := make(map[string]TemplateEntry)
	for _, root := range []struct {
		path   string
		source TemplateSource
	}{{p.globalDir, SourceGlobal}, {p.projectDir, SourceProject}} {
		found, err := p.scan(ctx, root.path, root.source)
		if err != nil {
			return err
		}
		for _, entry := range found {
			entries[entry.Name] = entry
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.entries = entries
	p.order = orderPreserving(previous, entries)
	p.mu.Unlock()
	return nil
}

// Index returns the cheap half of every template.
func (p *TemplatePool) Index() []TemplateEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entries := make([]TemplateEntry, 0, len(p.entries))
	seen := make(map[string]bool, len(p.order))
	for _, name := range p.order {
		if entry, ok := p.entries[name]; ok {
			entries = append(entries, entry)
			seen[name] = true
		}
	}
	// Anything the order does not mention is still a template, so a caller never
	// sees a pool that looks empty because the order was never built.
	if len(entries) != len(p.entries) {
		rest := make([]string, 0, len(p.entries))
		for name := range p.entries {
			if !seen[name] {
				rest = append(rest, name)
			}
		}
		sort.Strings(rest)
		for _, name := range rest {
			entries = append(entries, p.entries[name])
		}
	}
	return entries
}

// IndexText is the index as one line per template, which is what a system prompt
// carries. The instructions are deliberately absent: a caller has to choose a
// template before it pays for one, and a pool that front-loads every prompt spends
// the window on sub agents nobody wanted.
func (p *TemplatePool) IndexText() string {
	var builder strings.Builder
	for _, entry := range p.Index() {
		builder.WriteString(entry.Name)
		builder.WriteString(": ")
		builder.WriteString(entry.Description)
		if len(entry.Tools) > 0 {
			builder.WriteString(" (tools: ")
			builder.WriteString(strings.Join(entry.Tools, ", "))
			builder.WriteByte(')')
		}
		builder.WriteByte('\n')
	}
	return builder.String()
}

// Lookup returns the entry without reading the file.
func (p *TemplatePool) Lookup(name string) (TemplateEntry, bool) {
	name = strings.TrimSpace(name)
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.entries[name]
	return entry, ok
}

// Load reads a template's instructions.
//
// The file is resolved through symlinks and then checked to be inside the directory
// it was found in, because a template is a file a person can point anywhere and a
// main core that can be talked into reading a path is a main core that can be talked
// into reading anything.
func (p *TemplatePool) Load(ctx context.Context, name string) (Template, error) {
	if ctx == nil {
		return Template{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return Template{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Template{}, fmt.Errorf("%w: name must not be empty", ErrInvalidTemplate)
	}
	entry, ok := p.Lookup(name)
	if !ok {
		return Template{}, fmt.Errorf("%w: %s", ErrTemplateNotFound, name)
	}
	resolvedFile, err := filepath.EvalSymlinks(entry.File)
	if err != nil {
		return Template{}, fmt.Errorf("subagent: resolve template file %s: %w", entry.File, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(entry.Root)
	if err != nil {
		return Template{}, fmt.Errorf("subagent: resolve template root %s: %w", entry.Root, err)
	}
	if !pathWithin(resolvedRoot, resolvedFile) {
		return Template{}, fmt.Errorf("subagent: the template file %s is outside %s", resolvedFile, resolvedRoot)
	}
	info, err := os.Stat(resolvedFile)
	if err != nil {
		return Template{}, fmt.Errorf("subagent: stat template %s: %w", resolvedFile, err)
	}
	if info.Size() > p.maxBytes {
		return Template{}, fmt.Errorf("subagent: the template %s is %d bytes, over the %d limit",
			name, info.Size(), p.maxBytes)
	}
	data, err := os.ReadFile(resolvedFile)
	if err != nil {
		return Template{}, fmt.Errorf("subagent: read template %s: %w", resolvedFile, err)
	}
	if !utf8.Valid(data) {
		return Template{}, fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidTemplate, name)
	}
	front, body, err := parseTemplate(data)
	if err != nil {
		return Template{}, fmt.Errorf("%w: %s: %w", ErrInvalidTemplate, name, err)
	}
	if err := validateTemplate(front, entry.Name); err != nil {
		return Template{}, fmt.Errorf("%w: %s: %w", ErrInvalidTemplate, name, err)
	}
	return Template{
		TemplateEntry: entry,
		Instructions:  body,
	}, nil
}

// Search finds templates by name, description or keyword.
func (p *TemplatePool) Search(ctx context.Context, query string, limit int) ([]TemplateEntry, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, fmt.Errorf("subagent: a template search must not be empty")
	}
	if limit <= 0 || limit > p.maxResults {
		limit = p.maxResults
	}
	var found []TemplateEntry
	for _, entry := range p.Index() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if templateMatches(entry, query) {
			found = append(found, entry)
			if len(found) == limit {
				break
			}
		}
	}
	return found, nil
}

func templateMatches(entry TemplateEntry, query string) bool {
	if strings.Contains(strings.ToLower(entry.Name), query) ||
		strings.Contains(strings.ToLower(entry.Description), query) {
		return true
	}
	for _, keyword := range entry.Keywords {
		if strings.Contains(strings.ToLower(keyword), query) {
			return true
		}
	}
	return false
}

// scan walks one root and returns what it holds.
func (p *TemplatePool) scan(ctx context.Context, root string, source TemplateSource) ([]TemplateEntry, error) {
	if root == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("subagent: stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("subagent: %s is not a directory", root)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("subagent: resolve %s: %w", root, err)
	}
	items, err := os.ReadDir(realRoot)
	if err != nil {
		return nil, fmt.Errorf("subagent: read %s: %w", realRoot, err)
	}
	entries := make([]TemplateEntry, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A symlinked directory is skipped rather than followed. A template is
		// something a person may well want to keep somewhere else and point at, and
		// following one would let a link reach outside the directory that was named
		// as the boundary.
		if !item.IsDir() || item.Type()&os.ModeSymlink != 0 {
			continue
		}
		templateRoot := filepath.Join(realRoot, item.Name())
		resolvedRoot, err := filepath.EvalSymlinks(templateRoot)
		if err != nil {
			return nil, fmt.Errorf("subagent: resolve %s: %w", templateRoot, err)
		}
		file := filepath.Join(resolvedRoot, TemplateFileName)
		resolvedFile, err := filepath.EvalSymlinks(file)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// A directory without a template file is not a broken template, it is
				// a directory that holds something else.
				continue
			}
			return nil, fmt.Errorf("subagent: resolve %s: %w", file, err)
		}
		entry, err := p.readEntry(ctx, item.Name(), resolvedRoot, resolvedFile, source)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(first, second int) bool {
		return entries[first].Name < entries[second].Name
	})
	return entries, nil
}

// readEntry reads one template's header.
//
// The body is not read here. Scanning a directory of templates on every refresh
// would read every prompt in it, and a refresh happens often enough that the cost
// would show up in a latency gate for a file nobody has asked for.
func (p *TemplatePool) readEntry(
	ctx context.Context, dirName, root, file string, source TemplateSource,
) (TemplateEntry, error) {
	info, err := os.Stat(file)
	if err != nil {
		return TemplateEntry{}, fmt.Errorf("subagent: stat %s: %w", file, err)
	}
	if info.Size() > p.maxBytes {
		return TemplateEntry{}, fmt.Errorf("subagent: the template %s is %d bytes, over the %d limit",
			dirName, info.Size(), p.maxBytes)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return TemplateEntry{}, fmt.Errorf("subagent: read %s: %w", file, err)
	}
	if !utf8.Valid(data) {
		return TemplateEntry{}, fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidTemplate, dirName)
	}
	front, _, err := parseTemplate(data)
	if err != nil {
		return TemplateEntry{}, fmt.Errorf("%w: %s: %w", ErrInvalidTemplate, dirName, err)
	}
	// The header's own name is checked before it is compared with the directory,
	// because a name that is not usable at all is the mistake, and saying the two
	// disagree sends the reader to the wrong line of their own file.
	if err := validateTemplateName(front.Name); err != nil {
		return TemplateEntry{}, fmt.Errorf("%w: %s: %w", ErrInvalidTemplate, dirName, err)
	}
	// The directory is the name. A header that disagrees with where the file lives
	// is a file two people edited, and guessing which one wins is how a sub agent
	// ends up being the wrong one twice.
	if front.Name != dirName {
		return TemplateEntry{}, fmt.Errorf(
			"%w: %s is named %q in its header, so the two disagree",
			ErrInvalidTemplate, dirName, front.Name)
	}
	if err := validateTemplate(front, dirName); err != nil {
		return TemplateEntry{}, fmt.Errorf("%w: %s: %w", ErrInvalidTemplate, dirName, err)
	}
	return TemplateEntry{
		Name:          dirName,
		Description:   front.Description,
		Keywords:      normalizeList(front.Keywords),
		Tools:         normalizeList(front.Tools),
		Model:         strings.TrimSpace(front.Model),
		MaxIterations: front.MaxIterations,
		Source:        source,
		Root:          root,
		File:          file,
	}, nil
}

func normalizeTemplateRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", nil
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", root, err)
	}
	return filepath.Clean(absolute), nil
}

// orderPreserving keeps the order a caller has already seen and appends what is
// new, so a rescanned template does not move in an index that was cached.
func orderPreserving(previous []string, entries map[string]TemplateEntry) []string {
	ordered := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, name := range previous {
		if _, ok := entries[name]; ok && !seen[name] {
			ordered = append(ordered, name)
			seen[name] = true
		}
	}
	added := make([]string, 0, len(entries))
	for name := range entries {
		if !seen[name] {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	return append(ordered, added...)
}

func normalizeList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
