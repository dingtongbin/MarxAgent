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

// probeSeatbelt verifies that the host can actually apply a profile. The
// sandbox-exec shim is deprecated and newer systems abort instead of enforcing,
// so an installed binary is not evidence that commands are being isolated.
func probeSeatbelt(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	profile := "(version 1)\n(deny default)\n(allow file-read-metadata)\n(allow file-read*)\n" +
		"(allow process-fork)\n(allow process-exec*)\n(allow signal)\n(allow sysctl-read)\n(allow mach-lookup)\n"
	for _, candidate := range []string{"/usr/bin/true", "/bin/true", "/usr/bin/printf", "/bin/echo"} {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
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
	stdin, err := command.StdinPipe()
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return nil, err
	}
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(tempRoot)
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
