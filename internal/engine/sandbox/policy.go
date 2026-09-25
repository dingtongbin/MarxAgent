// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalidPolicy = errors.New("sandbox: invalid policy")
	ErrUnsupported   = errors.New("sandbox: backend is unavailable")
	ErrRequired      = errors.New("sandbox: a sandbox runner is required")
)

type Policy struct {
	Workspace         string
	ReadOnlyRoots     []string
	WritableRoots     []string
	ForbiddenRoots    []string
	Network           bool
	AllowLoopback     bool
	Subprocess        bool
	EnvAllowlist      []string
	Timeout           time.Duration
	WorkspaceWritable bool
}

type Capabilities struct {
	Backend     string
	Filesystem  bool
	Network     bool
	ProcessTree bool
	DefaultDeny bool
}

type normalizedPolicy struct {
	workspace         string
	readOnlyRoots     []string
	writableRoots     []string
	forbiddenRoots    []string
	network           bool
	allowLoopback     bool
	subprocess        bool
	envAllowlist      []string
	timeout           time.Duration
	workspaceWritable bool
}

type Runner interface {
	Start(context.Context, []string, []string, string) (Process, error)
	Capabilities() Capabilities
}

type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() (Result, error)
	Close() error
}

type Result struct {
	ExitCode int
}

type Engine struct {
	policy normalizedPolicy
}

func New(policy Policy) (*Engine, error) {
	if strings.TrimSpace(policy.Workspace) == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrInvalidPolicy)
	}
	normalized, err := normalizePolicy(policy)
	if err != nil {
		return nil, err
	}
	if !platformAvailable() {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, platformName())
	}
	if err := platformValidatePolicy(normalized); err != nil {
		return nil, err
	}
	return &Engine{policy: normalized}, nil
}

func NewUnrestricted() *Engine {
	return &Engine{policy: normalizedPolicy{workspace: "", envAllowlist: []string{}, timeout: 0}}
}

func (e *Engine) Capabilities() Capabilities {
	if e == nil {
		return Capabilities{}
	}
	return platformCapabilities()
}

func (e *Engine) Start(ctx context.Context, argv []string, env []string, dir string) (Process, error) {
	if e == nil {
		return nil, ErrRequired
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrInvalidPolicy)
	}
	if e.policy.workspace == "" {
		return startDirect(ctx, argv, env, dir)
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, fmt.Errorf("%w: command is required", ErrInvalidPolicy)
	}
	for _, value := range append(append([]string(nil), argv...), env...) {
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("%w: argument or environment contains NUL", ErrInvalidPolicy)
		}
	}
	if dir == "" {
		dir = e.policy.workspace
	}
	if !e.policy.allowsPath(dir) {
		return nil, fmt.Errorf("%w: working directory is outside the policy", ErrInvalidPolicy)
	}
	dir = canonicalPath(dir)
	runContext := ctx
	cancel := func() {}
	if e.policy.timeout > 0 {
		runContext, cancel = context.WithTimeout(ctx, e.policy.timeout)
	} else {
		runContext, cancel = context.WithCancel(ctx)
	}
	process, err := startPlatform(runContext, e, argv, e.filterEnvironment(env), dir)
	if err != nil {
		cancel()
		return nil, err
	}
	return &cancelProcess{Process: process, cancel: cancel}, nil
}

func (p normalizedPolicy) allowsPath(path string) bool {
	path = canonicalPath(path)
	if p.workspace != "" && pathWithin(p.workspace, path) {
		return true
	}
	for _, root := range p.readOnlyRoots {
		if pathWithin(root, path) {
			return true
		}
	}
	for _, root := range p.writableRoots {
		if pathWithin(root, path) {
			return true
		}
	}
	return false
}

func (e *Engine) Policy() Policy {
	if e == nil {
		return Policy{}
	}
	return Policy{
		Workspace:         e.policy.workspace,
		ReadOnlyRoots:     append([]string(nil), e.policy.readOnlyRoots...),
		WritableRoots:     append([]string(nil), e.policy.writableRoots...),
		ForbiddenRoots:    append([]string(nil), e.policy.forbiddenRoots...),
		Network:           e.policy.network,
		AllowLoopback:     e.policy.allowLoopback,
		Subprocess:        e.policy.subprocess,
		EnvAllowlist:      append([]string(nil), e.policy.envAllowlist...),
		Timeout:           e.policy.timeout,
		WorkspaceWritable: e.policy.workspaceWritable,
	}
}

func normalizePolicy(policy Policy) (normalizedPolicy, error) {
	workspace, err := normalizeDirectory(policy.Workspace, true)
	if err != nil {
		return normalizedPolicy{}, fmt.Errorf("%w: workspace: %v", ErrInvalidPolicy, err)
	}
	readOnlyRoots, err := normalizeRoots(policy.ReadOnlyRoots, false)
	if err != nil {
		return normalizedPolicy{}, fmt.Errorf("%w: read-only roots: %v", ErrInvalidPolicy, err)
	}
	writableRoots, err := normalizeRoots(policy.WritableRoots, true)
	if err != nil {
		return normalizedPolicy{}, fmt.Errorf("%w: writable roots: %v", ErrInvalidPolicy, err)
	}
	forbiddenRoots, err := normalizeRoots(policy.ForbiddenRoots, true)
	if err != nil {
		return normalizedPolicy{}, fmt.Errorf("%w: forbidden roots: %v", ErrInvalidPolicy, err)
	}
	if policy.AllowLoopback && !policy.Network {
		return normalizedPolicy{}, fmt.Errorf("%w: loopback requires network", ErrInvalidPolicy)
	}
	if policy.Timeout < 0 {
		return normalizedPolicy{}, fmt.Errorf("%w: timeout must not be negative", ErrInvalidPolicy)
	}
	if workspace != "" {
		if policy.WorkspaceWritable {
			writableRoots = addRoot(writableRoots, workspace)
		} else {
			readOnlyRoots = addRoot(readOnlyRoots, workspace)
		}
	}
	for _, forbidden := range forbiddenRoots {
		if workspace != "" && filepath.Clean(workspace) == filepath.Clean(forbidden) {
			return normalizedPolicy{}, fmt.Errorf("%w: workspace is forbidden", ErrInvalidPolicy)
		}
	}
	return normalizedPolicy{
		workspace:         workspace,
		readOnlyRoots:     readOnlyRoots,
		writableRoots:     writableRoots,
		forbiddenRoots:    forbiddenRoots,
		network:           policy.Network,
		allowLoopback:     policy.AllowLoopback,
		subprocess:        policy.Subprocess,
		envAllowlist:      normalizeEnvAllowlist(policy.EnvAllowlist),
		timeout:           policy.Timeout,
		workspaceWritable: policy.WorkspaceWritable,
	}, nil
}

func normalizeDirectory(path string, mustExist bool) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("path contains NUL")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if mustExist {
		info, err := os.Stat(absolute)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("path is not a directory")
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved), nil
	}
	return absolute, nil
}

func normalizeRoots(roots []string, mustExist bool) ([]string, error) {
	result := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		normalized, err := normalizeDirectory(root, mustExist)
		if err != nil {
			return nil, err
		}
		if normalized == "" {
			return nil, fmt.Errorf("root must not be empty")
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	sort.Strings(result)
	return result, nil
}

func addRoot(roots []string, root string) []string {
	for _, existing := range roots {
		if existing == root {
			return roots
		}
	}
	result := append(roots, root)
	sort.Strings(result)
	return result
}

// canonicalPath resolves a caller supplied path to the same form the policy
// roots were normalized to. Without it a caller that passes "/tmp/work" is
// rejected on macOS, where the real path is "/private/tmp/work", and Windows
// callers that pass 8.3 short names are rejected after expansion.
func canonicalPath(path string) string {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved)
	}
	return clean
}

func normalizeEnvAllowlist(values []string) []string {
	if len(values) == 0 {
		values = []string{"PATH", "HOME", "USERPROFILE", "SystemRoot", "windir", "ComSpec", "TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TERM"}
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "=\x00") {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (e *Engine) filterEnvironment(env []string) []string {
	source := env
	if source == nil {
		source = os.Environ()
	}
	values := make(map[string]string, len(source))
	for _, entry := range source {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			values[key] = value
		}
	}
	result := make([]string, 0, len(e.policy.envAllowlist)+4)
	for _, key := range e.policy.envAllowlist {
		if value, ok := values[key]; ok {
			result = append(result, key+"="+value)
		}
	}
	if _, ok := values["PATH"]; !ok {
		result = append(result, "PATH="+defaultPath())
	}
	return result
}
