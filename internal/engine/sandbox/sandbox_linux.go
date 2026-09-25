// SPDX-License-Identifier: Apache-2.0
//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func platformName() string {
	return "bubblewrap"
}

func platformAvailable() bool {
	path, err := exec.LookPath("bwrap")
	if err != nil || !trustedBackendPath(path) {
		return false
	}
	return probeBubblewrap(path)
}

// probeBubblewrap verifies that the host really lets bubblewrap build a user
// namespace. Hardened kernels and distribution policies can leave the binary
// installed while denying the namespace, and a sandbox that cannot isolate
// anything has to be reported as unsupported rather than quietly running
// commands with no isolation at all.
func probeBubblewrap(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, candidate := range []struct {
		file      string
		arguments []string
	}{
		{file: "/bin/sh", arguments: []string{"-c", ":"}},
		{file: "/usr/bin/sh", arguments: []string{"-c", ":"}},
		{file: "/bin/true"},
		{file: "/usr/bin/true"},
	} {
		if info, err := os.Stat(candidate.file); err != nil || info.IsDir() {
			continue
		}
		arguments := append([]string{
			"--unshare-user-try",
			"--unshare-pid",
			"--ro-bind", "/", "/",
			"--dev", "/dev",
			"--proc", "/proc",
			"--",
		}, append([]string{candidate.file}, candidate.arguments...)...)
		command := exec.CommandContext(ctx, path, arguments...)
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if command.Run() == nil {
			return true
		}
	}
	return false
}

func platformCapabilities() Capabilities {
	return Capabilities{Backend: platformName(), Filesystem: true, Network: true, ProcessTree: true, DefaultDeny: true}
}

func platformValidatePolicy(policy normalizedPolicy) error {
	if !policy.subprocess {
		return fmt.Errorf("%w: bubblewrap cannot deny subprocess creation", ErrInvalidPolicy)
	}
	return nil
}

func defaultPath() string {
	return "/usr/bin:/bin"
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func startPlatform(ctx context.Context, engine *Engine, argv, env []string, dir string) (Process, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil || !trustedBackendPath(bwrap) {
		return nil, fmt.Errorf("%w: trusted bubblewrap executable is required", ErrUnsupported)
	}
	executable, err := resolveExecutable(argv[0])
	if err != nil {
		return nil, err
	}
	tempRoot, err := os.MkdirTemp("", "marxagent-sandbox-")
	if err != nil {
		return nil, err
	}
	cleanupTemp := func() error { return os.RemoveAll(tempRoot) }
	args, err := buildBwrapArgs(engine, executable, argv, tempRoot, dir, env)
	if err != nil {
		_ = cleanupTemp()
		return nil, err
	}
	command := exec.CommandContext(ctx, bwrap, args...)
	command.Env = append([]string(nil), env...)
	command.Dir = dir
	stdin, err := command.StdinPipe()
	if err != nil {
		_ = cleanupTemp()
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = cleanupTemp()
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = cleanupTemp()
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = cleanupTemp()
		return nil, err
	}
	wait := func() (Result, error) {
		err := command.Wait()
		code := -1
		if command.ProcessState != nil {
			code = command.ProcessState.ExitCode()
		}
		return Result{ExitCode: code}, err
	}
	closeProcess := func() error {
		var err error
		if command.Process != nil && command.ProcessState == nil {
			err = command.Process.Kill()
		}
		return errorsJoin(err, cleanupTemp())
	}
	return newProcess(stdin, stdout, stderr, wait, closeProcess), nil
}

func buildBwrapArgs(engine *Engine, executable string, argv []string, tempRoot, dir string, env []string) ([]string, error) {
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup-try",
		"--unshare-user-try",
	}
	if !engine.policy.network {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--cap-drop", "ALL", "--clearenv")
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			args = append(args, "--setenv", key, value)
		}
	}
	args = append(args, "--dev", "/dev", "--proc", "/proc")
	// The scratch directory always lives under the host temporary directory, so
	// a fresh tmpfs over that directory would hide every host path exposed
	// below, including the workspace and the executable itself. Bind the
	// temporary directory read-only instead whenever anything we must expose
	// lives inside it, and keep the scratch directory writable.
	tempDir := canonicalPath(os.TempDir())
	args = append(args, tempDirFlags(engine, executable, dir, tempDir)...)
	args = append(args, "--bind", tempRoot, tempRoot)
	args = append(args, "--setenv", "TMPDIR", tempRoot, "--setenv", "TMP", tempRoot, "--setenv", "TEMP", tempRoot)
	seen := make(map[string]struct{})
	appendBind := func(flag, path string, required bool) error {
		if path == "" {
			return nil
		}
		if _, ok := seen[path]; ok {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			if required {
				return err
			}
			return nil
		}
		seen[path] = struct{}{}
		args = append(args, flag, path, path)
		_ = info
		return nil
	}
	for _, path := range linuxSystemReadRoots() {
		if err := appendBind("--ro-bind-try", path, false); err != nil {
			return nil, err
		}
	}
	for _, path := range engine.policy.writableRoots {
		if err := appendBind("--bind", path, true); err != nil {
			return nil, err
		}
	}
	for _, path := range engine.policy.readOnlyRoots {
		if err := appendBind("--ro-bind-try", path, false); err != nil {
			return nil, err
		}
	}
	if !pathVisible(executable, seen) {
		if err := appendBind("--ro-bind-try", filepath.Dir(executable), false); err != nil {
			return nil, err
		}
	}
	if dir != "" && !pathVisible(dir, seen) {
		if err := appendBind("--ro-bind-try", dir, false); err != nil {
			return nil, err
		}
	}
	for _, path := range engine.policy.forbiddenRoots {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			args = append(args, "--tmpfs", path)
		} else {
			args = append(args, "--ro-bind", os.DevNull, path)
		}
	}
	args = append(args, "--chdir", dir)
	args = append(args, "--", executable)
	args = append(args, argv[1:]...)
	return args, nil
}

// tempDirFlags decides how the host temporary directory is exposed. A private
// tmpfs is the stronger default, but it is only usable when no path that must
// stay visible lives underneath it.
func tempDirFlags(engine *Engine, executable, dir, tempDir string) []string {
	exposed := []string{executable, dir}
	exposed = append(exposed, engine.policy.workspace)
	exposed = append(exposed, engine.policy.writableRoots...)
	exposed = append(exposed, engine.policy.readOnlyRoots...)
	for _, path := range exposed {
		if path == "" {
			continue
		}
		if pathWithin(tempDir, canonicalPath(path)) {
			return []string{"--ro-bind", tempDir, tempDir}
		}
	}
	return []string{"--tmpfs", tempDir}
}

func trustedBackendPath(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, root := range []string{"/usr", "/bin", "/sbin"} {
		if pathWithin(root, path) {
			return true
		}
	}
	return false
}

func linuxSystemReadRoots() []string {
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/etc/alternatives", "/etc/ssl", "/etc/ca-certificates", "/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf"}
}

func pathVisible(path string, roots map[string]struct{}) bool {
	clean := filepath.Clean(path)
	for root := range roots {
		if pathWithin(root, clean) {
			return true
		}
	}
	return false
}
