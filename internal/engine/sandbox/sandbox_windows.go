// SPDX-License-Identifier: Apache-2.0
//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	winsandbox "github.com/SivanCola/windows-sandbox"
)

var windowsSandboxEnvMu sync.Mutex

func platformName() string {
	return "windows-appcontainer"
}

func platformAvailable() bool {
	return winsandbox.Available()
}

func platformCapabilities() Capabilities {
	return Capabilities{Backend: platformName(), Filesystem: true, Network: true, ProcessTree: true, DefaultDeny: true}
}

func platformValidatePolicy(policy normalizedPolicy) error {
	if policy.timeout <= 0 {
		return fmt.Errorf("%w: Windows sandbox requires a positive timeout", ErrInvalidPolicy)
	}
	if !policy.subprocess {
		return fmt.Errorf("%w: Windows backend cannot deny subprocess creation", ErrInvalidPolicy)
	}
	if len(policy.writableRoots) > 0 && !policy.network {
		return fmt.Errorf("%w: Windows writable sandbox cannot deny network", ErrInvalidPolicy)
	}
	if len(policy.writableRoots) > 0 && len(policy.readOnlyRoots) > 0 {
		return fmt.Errorf("%w: Windows backend cannot mix writable and read-only roots", ErrInvalidPolicy)
	}
	return nil
}

func defaultPath() string {
	return `C:\Windows\System32;C:\Windows`
}

func pathWithin(root, target string) bool {
	root = strings.ToLower(filepath.Clean(root))
	target = strings.ToLower(filepath.Clean(target))
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// platformRoots collects every directory the AppContainer is granted. The
// sandbox backend rewrites the DACL of each root it is given, so only
// directories this process may modify can be listed: system directories such as
// the PowerShell home are deliberately absent and are reached through the
// default AppContainer access instead.
func platformRoots(engine *Engine, dir string) []string {
	roots := make([]string, 0, 8)
	add := func(path string) {
		if path == "" {
			return
		}
		canonical := canonicalPath(path)
		for _, existing := range roots {
			if pathWithin(existing, canonical) {
				return
			}
		}
		roots = append(roots, canonical)
	}
	add(engine.policy.workspace)
	for _, root := range engine.policy.readOnlyRoots {
		add(root)
	}
	for _, root := range engine.policy.writableRoots {
		add(root)
	}
	add(dir)
	return roots
}

func startPlatform(ctx context.Context, engine *Engine, argv, env []string, dir string) (Process, error) {
	if !winsandbox.Available() {
		return nil, fmt.Errorf("%w: Windows AppContainer APIs are unavailable", ErrUnsupported)
	}
	if engine.policy.timeout <= 0 {
		return nil, fmt.Errorf("%w: Windows sandbox requires a positive timeout", ErrInvalidPolicy)
	}
	writable := len(engine.policy.writableRoots) > 0
	if writable && !engine.policy.network {
		return nil, fmt.Errorf("%w: Windows writable sandbox cannot deny network", ErrInvalidPolicy)
	}
	executable, err := resolveExecutable(argv[0])
	if err != nil {
		return nil, err
	}
	roots := platformRoots(engine, dir)
	if len(roots) == 0 {
		return nil, fmt.Errorf("%w: Windows sandbox has no allowed roots", ErrInvalidPolicy)
	}
	// The AppContainer inherits a filtered environment, so the command is
	// resolved here and passed as an absolute path.
	command := append([]string{executable}, argv[1:]...)
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		return nil, err
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return nil, err
	}
	resultChannel := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		windowsSandboxEnvMu.Lock()
		previous, hadPrevious := os.LookupEnv("WINDOWS_SANDBOX_WAIT_MS")
		_ = os.Setenv("WINDOWS_SANDBOX_WAIT_MS", strconv.FormatInt(engine.policy.timeout.Milliseconds(), 10))
		allowedRoots := append([]string(nil), roots...)
		result, runErr := winsandbox.Run(winsandbox.Spec{
			WritableRoots:   allowedRoots,
			ForbidReadRoots: append([]string(nil), engine.policy.forbiddenRoots...),
			Network:         engine.policy.network,
			Writable:        writable,
			TempPrefix:      "marxagent-sandbox-",
		}, append([]string(nil), command...), winsandbox.RunOptions{
			Stdin:  stdinReader,
			Stdout: stdoutWriter,
			Stderr: stderrWriter,
			Env:    append([]string(nil), env...),
			Dir:    dir,
		})
		if hadPrevious {
			_ = os.Setenv("WINDOWS_SANDBOX_WAIT_MS", previous)
		} else {
			_ = os.Unsetenv("WINDOWS_SANDBOX_WAIT_MS")
		}
		windowsSandboxEnvMu.Unlock()
		_ = stdinReader.Close()
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		resultChannel <- struct {
			result Result
			err    error
		}{Result{ExitCode: result.ExitCode}, runErr}
	}()
	wait := func() (Result, error) {
		select {
		case value := <-resultChannel:
			return value.result, value.err
		case <-ctx.Done():
			return Result{ExitCode: -1}, ctx.Err()
		}
	}
	closeProcess := func() error {
		return nil
	}
	return newProcess(stdinWriter, stdoutReader, stderrReader, wait, closeProcess), nil
}
