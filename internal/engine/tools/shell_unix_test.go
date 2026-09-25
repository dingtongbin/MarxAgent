// SPDX-License-Identifier: Apache-2.0
//go:build !windows

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

func TestShellExecutesWithUnixShell(t *testing.T) {
	workspace := t.TempDir()
	engine, err := sandbox.New(sandbox.Policy{Workspace: workspace, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 60 * time.Second})
	if err != nil {
		if errors.Is(err, sandbox.ErrUnsupported) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	tool, err := NewShellTool(ShellConfig{Shell: "/bin/sh", WorkingDirectory: workspace, Sandbox: engine})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	result, err := sandbox.Run(context.Background(), engine, []string{"/bin/sh", "-c", "printf shell-ok"}, nil, workspace, nil, &stdout, &stderr)
	if err != nil || result.ExitCode != 0 || stdout.String() != "shell-ok" {
		t.Fatalf("sandbox run failed: err = %v result = %#v stdout = %q stderr = %q", err, result, stdout.String(), stderr.String())
	}
	toolResult, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf shell-ok"}`))
	if err != nil {
		t.Fatalf("shell execution failed: %v", err)
	}
	if toolResult.IsError || !strings.Contains(string(toolResult.Output), "shell-ok") {
		t.Fatalf("result = %#v", toolResult)
	}
}

func TestShellReportsNonZeroExit(t *testing.T) {
	tool, err := NewShellTool(ShellConfig{Shell: "/bin/sh", Sandbox: sandbox.NewUnrestricted()})
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
