// SPDX-License-Identifier: Apache-2.0
//go:build darwin

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
	return "seatbelt"
}

func platformAvailable() bool {
	path, err := exec.LookPath("sandbox-exec")
	if err != nil || !trustedBackendPath(path) {
		return false
	}
	return probeSeatbelt(path)
}

// probeSeatbelt verifies that the host can enforce the profile this package
// actually generates. The sandbox-exec shim is deprecated, newer systems abort
// instead of enforcing, and a probe with a different profile would report a
// backend as usable while every real command dies with SIGABRT.
func probeSeatbelt(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	workspace, err := os.MkdirTemp("", "marxagent-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(workspace)
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		resolved = workspace
	}
	engine := &Engine{policy: normalizedPolicy{
		workspace:     resolved,
		writableRoots: []string{resolved},
		readOnlyRoots: darwinSystemReadRoots(),
		subprocess:    true,
		network:       true,
		timeout:       10 * time.Second,
		envAllowlist:  []string{"PATH"},
	}}
	for _, candidate := range []string{"/bin/sh", "/usr/bin/true", "/usr/bin/printf"} {
		info, statErr := os.Stat(candidate)
		if statErr != nil || info.IsDir() {
			continue
		}
		profile, buildErr := buildSBPL(engine, candidate, resolved)
		if buildErr != nil {
			return false
		}
		command := exec.CommandContext(ctx, path, "-p", profile, candidate)
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

func platformValidatePolicy(normalizedPolicy) error {
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
	sandboxExec, err := exec.LookPath("sandbox-exec")
	if err != nil || !trustedBackendPath(sandboxExec) {
		return nil, fmt.Errorf("%w: trusted sandbox-exec is required", ErrUnsupported)
	}
	executable, err := resolveExecutable(argv[0])
	if err != nil {
		return nil, err
	}
	tempRoot, err := os.MkdirTemp("", "marxagent-sandbox-")
	if err != nil {
		return nil, err
	}
	profile, err := buildSBPL(engine, executable, tempRoot)
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	command := exec.CommandContext(ctx, sandboxExec, "-p", profile, executable)
	command.Args = append(command.Args, argv[1:]...)
	command.Env = append([]string(nil), env...)
	command.Dir = dir
	// The pipes are made here rather than with Cmd.StdoutPipe, which closes the
	// reading end as soon as Wait sees the process exit. Whatever the command wrote
	// and nobody had read yet is discarded at that point, so a process that exits
	// promptly can report a clean exit code and no output at all. Holding the reading
	// end here also means Wait has no opinion about it, so a caller reading
	// incrementally gets every byte the process wrote.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	stdinReader, stdin, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	command.Stdin = stdinReader
	if err := command.Start(); err != nil {
		_ = stdinReader.Close()
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	// The child holds the reading and writing ends it needs now. Closing this
	// process's copies is what lets a reader see the end of the stream when the
	// child is done.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	_ = stdinReader.Close()
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
		return errorsJoin(err, os.RemoveAll(tempRoot))
	}
	return newProcess(stdin, stdout, stderr, wait, closeProcess), nil
}

func buildSBPL(engine *Engine, executable, tempRoot string) (string, error) {
	var builder strings.Builder
	builder.WriteString("(version 1)\n(deny default)\n")
	// Seatbelt evaluates every path component, so a process cannot even traverse
	// into an allowed subpath unless metadata reads are permitted. Without this
	// rule the sandboxed binary aborts before it reaches its entry point.
	builder.WriteString("(allow file-read-metadata)\n")
	builder.WriteString("(allow signal)\n")
	builder.WriteString("(allow sysctl-read)\n")
	builder.WriteString("(allow mach-lookup)\n")
	builder.WriteString("(allow file-ioctl)\n")
	if engine.policy.subprocess {
		builder.WriteString("(allow process-fork)\n(allow process-exec*)\n")
	} else {
		builder.WriteString("(deny process-fork)\n")
		if err := writeSBPLRule(&builder, "allow", "process-exec*", executable); err != nil {
			return "", err
		}
	}
	for _, path := range darwinSystemReadRoots() {
		if err := writeSBPLRule(&builder, "allow", "file-read*", path); err != nil {
			return "", err
		}
	}
	if err := writeSBPLRule(&builder, "allow", "file-read*", engine.policy.workspace); err != nil {
		return "", err
	}
	for _, path := range engine.policy.readOnlyRoots {
		if err := writeSBPLRule(&builder, "allow", "file-read*", path); err != nil {
			return "", err
		}
	}
	for _, path := range engine.policy.writableRoots {
		if err := writeSBPLRule(&builder, "allow", "file-read*", path); err != nil {
			return "", err
		}
		if err := writeSBPLRule(&builder, "allow", "file-write*", path); err != nil {
			return "", err
		}
	}
	if err := writeSBPLRule(&builder, "allow", "file-read*", tempRoot); err != nil {
		return "", err
	}
	if err := writeSBPLRule(&builder, "allow", "file-write*", tempRoot); err != nil {
		return "", err
	}
	for _, path := range engine.policy.forbiddenRoots {
		if err := writeSBPLRule(&builder, "deny", "file-read*", path); err != nil {
			return "", err
		}
		if err := writeSBPLRule(&builder, "deny", "file-write*", path); err != nil {
			return "", err
		}
	}
	if engine.policy.network {
		builder.WriteString("(allow network*)\n")
	}
	return builder.String(), nil
}

func writeSBPLRule(builder *strings.Builder, action, operation, path string) error {
	if err := validateSBPLPath(path); err != nil {
		return err
	}
	fmt.Fprintf(builder, "(%s %s (subpath %q))\n", action, operation, path)
	return nil
}

func validateSBPLPath(path string) error {
	if path == "" {
		return fmt.Errorf("%w: empty sandbox path", ErrInvalidPolicy)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("%w: sandbox path contains NUL", ErrInvalidPolicy)
	}
	for _, character := range path {
		if character < 0x20 || character == 0x7f || strings.ContainsRune("\"\\();'", character) {
			return fmt.Errorf("%w: unsafe sandbox path %q", ErrInvalidPolicy, path)
		}
	}
	return nil
}

func trustedBackendPath(path string) bool {
	return filepath.IsAbs(path) && pathWithin("/usr", path)
}

func darwinSystemReadRoots() []string {
	return []string{"/usr", "/bin", "/sbin", "/System", "/Library", "/private/var/db/dyld", "/private/etc", "/etc", "/dev"}
}
