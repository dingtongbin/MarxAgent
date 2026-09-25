// SPDX-License-Identifier: Apache-2.0
//go:build windows

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
)

// TestShellExecutesWithWindowsPowerShell exercises the default Windows shell of
// the tool. PowerShell needs to read the .NET facades under C:\Windows, which
// some Server images deny to the AppContainer, so the host is probed first and
// the case reports a skip instead of a sandbox failure.
func TestShellExecutesWithWindowsPowerShell(t *testing.T) {
	workspace := t.TempDir()
	engine, err := sandbox.New(sandbox.Policy{Workspace: workspace, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 60 * time.Second})
	if err != nil {
		if errors.Is(err, sandbox.ErrUnsupported) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if !powerShellRunsInToolSandbox(t, engine, workspace) {
		return
	}
	tool, err := NewShellTool(ShellConfig{Shell: "powershell.exe", WorkingDirectory: workspace, Sandbox: engine})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"Write-Output shell-ok"}`))
	if err != nil {
		t.Fatalf("shell execution failed: %v", err)
	}
	if result.IsError || !strings.Contains(string(result.Output), "shell-ok") {
		t.Fatalf("result = %#v", result)
	}
}

func powerShellRunsInToolSandbox(t *testing.T, engine sandbox.Runner, workspace string) bool {
	t.Helper()
	var stdout, stderr strings.Builder
	result, err := sandbox.Run(context.Background(), engine, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Write-Output ready"}, nil, workspace, nil, &stdout, &stderr)
	if err == nil && result.ExitCode == 0 && strings.Contains(stdout.String(), "ready") {
		return true
	}
	t.Logf("SKIP: PowerShell cannot start inside the AppContainer on this host: err = %v result = %#v stderr = %q", err, result, stderr.String())
	return false
}

func TestShellArgsResolvesPowerShellAndRejectsMissingShell(t *testing.T) {
	args, err := shellArgs(ShellConfig{}, "Write-Output ok")
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 6 || args[0] == "" || args[5] != "Write-Output ok" {
		t.Fatalf("args = %#v", args)
	}
	if args[1] != "-NoLogo" || args[2] != "-NoProfile" || args[3] != "-NonInteractive" || args[4] != "-Command" {
		t.Fatalf("args = %#v", args)
	}
	explicit, err := shellArgs(ShellConfig{Shell: "custom-shell.exe"}, "noop")
	if err != nil {
		t.Fatal(err)
	}
	if explicit[0] != "custom-shell.exe" {
		t.Fatalf("explicit shell ignored: %#v", explicit)
	}
	t.Setenv("PATH", "")
	if _, err := shellArgs(ShellConfig{}, "noop"); err == nil || !strings.Contains(err.Error(), "PowerShell") {
		t.Fatalf("missing PowerShell error = %v", err)
	}
}

func TestShellReportsNonZeroExit(t *testing.T) {
	tool, err := NewShellTool(ShellConfig{Shell: "powershell.exe", Sandbox: sandbox.NewUnrestricted()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"exit 7"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(string(result.Output), `"exit_code":7`) {
		t.Fatalf("result = %#v", result)
	}
}
