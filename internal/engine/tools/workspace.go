// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

const (
	defaultWorkspaceMaxFileBytes int64 = 8 << 20
	defaultWorkspaceMaxResults         = 1000
)

type WorkspaceOptions struct {
	MaxFileBytes int64
	MaxResults   int
}

type Workspace struct {
	root         string
	maxFileBytes int64
	maxResults   int
}

func NewWorkspace(root string, options WorkspaceOptions) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("tools: workspace root must not be empty")
	}
	if strings.IndexByte(root, 0) >= 0 {
		return nil, fmt.Errorf("tools: workspace root contains NUL")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("tools: resolve workspace root: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("tools: stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("tools: workspace root is not a directory")
	}
	realRoot, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("tools: resolve workspace root: %w", err)
	}
	maxFileBytes := options.MaxFileBytes
	if maxFileBytes == 0 {
		maxFileBytes = defaultWorkspaceMaxFileBytes
	}
	if maxFileBytes < 1 {
		return nil, fmt.Errorf("tools: max file bytes must be positive")
	}
	maxResults := options.MaxResults
	if maxResults == 0 {
		maxResults = defaultWorkspaceMaxResults
	}
	if maxResults < 1 {
		return nil, fmt.Errorf("tools: max results must be positive")
	}
	return &Workspace{root: realRoot, maxFileBytes: maxFileBytes, maxResults: maxResults}, nil
}

func (w *Workspace) Root() string {
	if w == nil {
		return ""
	}
	return w.root
}

func (w *Workspace) BuiltinTools() []core.Tool {
	if w == nil {
		return nil
	}
	tools := w.FileTools()
	return append(tools, w.SearchTools()...)
}

func (w *Workspace) resolveRead(path string) (string, error) {
	if w == nil {
		return "", fmt.Errorf("tools: nil workspace")
	}
	candidate, err := w.candidate(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("tools: resolve path %q: %w", path, err)
	}
	if !pathWithin(w.root, resolved) {
		return "", fmt.Errorf("tools: path %q escapes workspace", path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("tools: path %q is not a regular file", path)
	}
	return resolved, nil
}

func (w *Workspace) resolveDirectory(path string) (string, error) {
	if w == nil {
		return "", fmt.Errorf("tools: nil workspace")
	}
	candidate, err := w.candidate(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("tools: resolve directory %q: %w", path, err)
	}
	if !pathWithin(w.root, resolved) {
		return "", fmt.Errorf("tools: directory %q escapes workspace", path)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("tools: path %q is not a directory", path)
	}
	return resolved, nil
}

func (w *Workspace) prepareWrite(path string) (string, error) {
	if w == nil {
		return "", fmt.Errorf("tools: nil workspace")
	}
	candidate, err := w.candidate(path)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Lstat(candidate); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("tools: refusing to write through symlink %q", path)
		}
		if info.IsDir() {
			return "", fmt.Errorf("tools: path %q is a directory", path)
		}
	} else if !os.IsNotExist(statErr) {
		return "", statErr
	}
	parent := filepath.Dir(candidate)
	if err := w.checkWriteParents(parent); err != nil {
		return "", err
	}
	return candidate, nil
}

func (w *Workspace) candidate(path string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("tools: path must not be empty or contain NUL")
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(w.root, candidate)
	}
	candidate = canonicalPath(candidate)
	if !pathWithin(w.root, candidate) {
		return "", fmt.Errorf("tools: path %q escapes workspace", path)
	}
	return candidate, nil
}

// canonicalPath resolves symlinks so a caller supplied path compares equal to
// the resolved workspace root. macOS reports "/tmp/..." as "/private/tmp/..."
// and Windows expands 8.3 short names, both of which would otherwise look like
// workspace escapes.
func canonicalPath(path string) string {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved)
	}
	return clean
}

func (w *Workspace) checkWriteParents(parent string) error {
	rel, err := filepath.Rel(w.root, parent)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("tools: path escapes workspace")
	}
	current := w.root
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("tools: refusing to write through symlink %q", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("tools: path component %q is not a directory", current)
		}
	}
	return nil
}

func (w *Workspace) readFile(ctx context.Context, path string) ([]byte, error) {
	if w == nil {
		return nil, fmt.Errorf("tools: nil workspace")
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, w.maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > w.maxFileBytes {
		return nil, fmt.Errorf("tools: file exceeds %d bytes", w.maxFileBytes)
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

func (w *Workspace) writeFile(ctx context.Context, path string, data []byte) error {
	if w == nil {
		return fmt.Errorf("tools: nil workspace")
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if int64(len(data)) > w.maxFileBytes {
		return fmt.Errorf("tools: content exceeds %d bytes", w.maxFileBytes)
	}
	candidate, err := w.prepareWrite(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(candidate), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(candidate); statErr == nil {
		mode = info.Mode().Perm()
	}
	file, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return contextError(ctx)
}

func (w *Workspace) relative(path string) string {
	rel, err := filepath.Rel(w.root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("tools: context must not be nil")
	}
	return ctx.Err()
}
