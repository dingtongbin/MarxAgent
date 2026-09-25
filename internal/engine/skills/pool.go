// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const (
	defaultMaxFileBytes int64 = 2 << 20
	defaultMaxResults         = 32
	maxInt64                  = int64(^uint64(0) >> 1)
)

var (
	ErrNilContext       = errors.New("skills: context must not be nil")
	ErrSkillNotFound    = errors.New("skills: skill not found")
	ErrInvalidSkill     = errors.New("skills: invalid skill")
	ErrResourceNotFound = errors.New("skills: resource not found")
	ErrResourceOutside  = errors.New("skills: resource path escapes skill root")
)

type Pool struct {
	globalDir  string
	projectDir string
	maxBytes   int64
	maxResults int

	refreshMu sync.Mutex
	mu        sync.RWMutex
	entries   map[string]Entry
	order     []string
}

func NewPool(config Config) (*Pool, error) {
	globalDir, err := normalizeRoot(config.GlobalDir)
	if err != nil {
		return nil, fmt.Errorf("skills: global directory: %w", err)
	}
	projectDir, err := normalizeRoot(config.ProjectDir)
	if err != nil {
		return nil, fmt.Errorf("skills: project directory: %w", err)
	}
	maxBytes := config.MaxFileBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxFileBytes
	}
	if maxBytes < 1 {
		return nil, fmt.Errorf("skills: max file bytes must be positive")
	}
	maxResults := config.MaxResults
	if maxResults == 0 {
		maxResults = defaultMaxResults
	}
	if maxResults < 1 {
		return nil, fmt.Errorf("skills: max results must be positive")
	}
	return &Pool{
		globalDir:  globalDir,
		projectDir: projectDir,
		maxBytes:   maxBytes,
		maxResults: maxResults,
		entries:    make(map[string]Entry),
	}, nil
}

func (p *Pool) Refresh(ctx context.Context) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	p.mu.RLock()
	previousOrder := append([]string(nil), p.order...)
	p.mu.RUnlock()
	entries := make(map[string]Entry)
	globalEntries, err := p.scanRoot(ctx, p.globalDir, SourceGlobal)
	if err != nil {
		return err
	}
	for _, entry := range globalEntries {
		entries[entry.Name] = entry
	}
	projectEntries, err := p.scanRoot(ctx, p.projectDir, SourceProject)
	if err != nil {
		return err
	}
	for _, entry := range projectEntries {
		entries[entry.Name] = entry
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.entries = entries
	p.order = nextOrder(previousOrder, entries)
	p.mu.Unlock()
	return nil
}

func (p *Pool) Index() []Entry {
	p.mu.RLock()
	entries := make([]Entry, 0, len(p.entries))
	ordered := len(p.order) != 0
	if !ordered {
		for _, entry := range p.entries {
			entries = append(entries, cloneEntry(entry))
		}
	} else {
		for _, name := range p.order {
			if entry, ok := p.entries[name]; ok {
				entries = append(entries, cloneEntry(entry))
			}
		}
	}
	p.mu.RUnlock()
	if !ordered {
		sort.Slice(entries, func(left, right int) bool {
			return entries[left].Name < entries[right].Name
		})
	}
	return entries
}

func (p *Pool) IndexText() string {
	var builder strings.Builder
	for _, entry := range p.Index() {
		builder.WriteString(entry.Name)
		builder.WriteString(": ")
		builder.WriteString(entry.Description)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func (p *Pool) Search(ctx context.Context, query string, limit int) ([]Entry, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("skills: query must not be empty")
	}
	if limit <= 0 || limit > p.maxResults {
		limit = p.maxResults
	}
	terms := tokenize(query)
	if len(terms) == 0 {
		return nil, fmt.Errorf("skills: query must contain letters or numbers")
	}
	results := make([]Entry, 0)
	for _, entry := range p.Index() {
		score := scoreEntry(entry, query, terms)
		if score > 0 {
			results = append(results, entry)
		}
	}
	sort.SliceStable(results, func(left, right int) bool {
		leftScore := scoreEntry(results[left], query, terms)
		rightScore := scoreEntry(results[right], query, terms)
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return results[left].Name < results[right].Name
	})
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (p *Pool) Activate(ctx context.Context, name string) (Skill, error) {
	if ctx == nil {
		return Skill{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return Skill{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Skill{}, fmt.Errorf("%w: name must not be empty", ErrInvalidSkill)
	}
	p.mu.RLock()
	entry, ok := p.entries[name]
	p.mu.RUnlock()
	if !ok {
		return Skill{}, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	resolvedFile, err := filepath.EvalSymlinks(entry.File)
	if err != nil {
		return Skill{}, fmt.Errorf("skills: resolve skill file %s: %w", entry.File, err)
	}
	if !pathWithin(entry.Root, resolvedFile) {
		return Skill{}, fmt.Errorf("%w: skill file escapes root %s", ErrInvalidSkill, entry.File)
	}
	front, body, err := p.readSkillFile(ctx, resolvedFile)
	if err != nil {
		return Skill{}, err
	}
	parsed, err := p.parseEntry(entry.Root, resolvedFile, front, Source(entry.Source))
	if err != nil {
		return Skill{}, err
	}
	if parsed.Name != name {
		return Skill{}, fmt.Errorf("%w: file %s changed name", ErrInvalidSkill, entry.File)
	}
	return Skill{Entry: cloneEntry(parsed), Instructions: string(body)}, nil
}

func (p *Pool) ReadResource(ctx context.Context, name, relative string) ([]byte, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	entry, ok := p.entries[strings.TrimSpace(name)]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSkillNotFound, name)
	}
	path, err := resourcePath(entry.Root, relative)
	if err != nil {
		return nil, err
	}
	data, err := p.readFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, fmt.Errorf("%w: %s", ErrResourceNotFound, relative)
		}
		return nil, err
	}
	return data, nil
}

func (p *Pool) scanRoot(ctx context.Context, root string, source Source) ([]Entry, error) {
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
		return nil, fmt.Errorf("skills: stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skills: root is not a directory: %s", root)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("skills: resolve root %s: %w", root, err)
	}
	items, err := os.ReadDir(realRoot)
	if err != nil {
		return nil, fmt.Errorf("skills: read root %s: %w", realRoot, err)
	}
	entries := make([]Entry, 0, len(items))
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !item.IsDir() || item.Type()&os.ModeSymlink != 0 {
			continue
		}
		skillRoot := filepath.Join(realRoot, item.Name())
		resolvedRoot, err := filepath.EvalSymlinks(skillRoot)
		if err != nil {
			return nil, fmt.Errorf("skills: resolve skill directory %s: %w", skillRoot, err)
		}
		if !pathWithin(realRoot, resolvedRoot) {
			return nil, fmt.Errorf("%w: skill directory escapes root %s", ErrInvalidSkill, skillRoot)
		}
		skillFile := filepath.Join(resolvedRoot, "SKILL.md")
		resolvedFile, err := filepath.EvalSymlinks(skillFile)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("skills: resolve skill file %s: %w", skillFile, err)
		}
		if !pathWithin(resolvedRoot, resolvedFile) {
			return nil, fmt.Errorf("%w: skill file escapes root %s", ErrInvalidSkill, skillFile)
		}
		front, _, err := p.readSkillFile(ctx, resolvedFile)
		if err != nil {
			return nil, err
		}
		entry, err := p.parseEntry(resolvedRoot, resolvedFile, front, source)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].Name < entries[right].Name
	})
	return entries, nil
}

func nextOrder(previous []string, entries map[string]Entry) []string {
	order := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, name := range previous {
		if _, ok := entries[name]; !ok {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		order = append(order, name)
	}
	remaining := make([]string, 0, len(entries)-len(order))
	for name := range entries {
		if _, exists := seen[name]; !exists {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	return append(order, remaining...)
}

func normalizeRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	if strings.IndexByte(root, 0) >= 0 {
		return "", fmt.Errorf("path contains NUL")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func cloneEntry(entry Entry) Entry {
	entry.Metadata = cloneMetadata(entry.Metadata)
	entry.Keywords = append([]string(nil), entry.Keywords...)
	return entry
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return nil
	}
	clone := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	return clone
}

func readFileLimited(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	readLimit := maxBytes
	if maxBytes < maxInt64 {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(file, readLimit))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds %d bytes: %s", maxBytes, path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func (p *Pool) readFile(ctx context.Context, path string) ([]byte, error) {
	return readFileLimited(ctx, path, p.maxBytes)
}

func (p *Pool) readSkillFile(ctx context.Context, path string) ([]byte, []byte, error) {
	data, err := p.readFile(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	front, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %v", ErrInvalidSkill, path, err)
	}
	return front, body, nil
}

func parseFrontmatter(data []byte, out *frontmatter) error {
	if err := yamlUnmarshal(data, out); err != nil {
		return err
	}
	if err := validateFrontmatter(*out); err != nil {
		return err
	}
	return nil
}

func validateFrontmatter(front frontmatter) error {
	if err := validateSkillName(front.Name); err != nil {
		return err
	}
	description := strings.TrimSpace(front.Description)
	if description == "" {
		return fmt.Errorf("description must not be empty")
	}
	if utf8.RuneCountInString(description) > 1024 {
		return fmt.Errorf("description exceeds 1024 characters")
	}
	if front.Compatibility != "" && utf8.RuneCountInString(front.Compatibility) > 500 {
		return fmt.Errorf("compatibility exceeds 500 characters")
	}
	if len(front.Keywords) > 32 {
		return fmt.Errorf("keywords exceed 32 entries")
	}
	for _, keyword := range front.Keywords {
		if strings.TrimSpace(keyword) == "" {
			return fmt.Errorf("keywords must not be empty")
		}
	}
	return nil
}

func validateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("name exceeds 64 characters")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return fmt.Errorf("name has invalid hyphen placement: %q", name)
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return fmt.Errorf("name contains invalid character %q", character)
	}
	return nil
}

func normalizeKeywords(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func tokenize(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsDigit(character)
	})
}

func scoreEntry(entry Entry, query string, terms []string) int {
	lowerQuery := strings.ToLower(strings.TrimSpace(query))
	name := strings.ToLower(entry.Name)
	description := strings.ToLower(entry.Description)
	compatibility := strings.ToLower(entry.Compatibility)
	keywords := make([]string, 0, len(entry.Keywords))
	for _, keyword := range entry.Keywords {
		keywords = append(keywords, strings.ToLower(keyword))
	}
	score := 0
	if strings.Contains(name, lowerQuery) {
		score += 8
	}
	if strings.Contains(description, lowerQuery) {
		score += 4
	}
	for _, term := range terms {
		if strings.Contains(name, term) {
			score += 3
		}
		if strings.Contains(description, term) {
			score++
		}
		if strings.Contains(compatibility, term) {
			score++
		}
		for _, keyword := range keywords {
			if strings.Contains(keyword, term) {
				score += 2
			}
		}
	}
	return score
}
