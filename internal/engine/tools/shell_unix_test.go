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
	engine, err := sandbox.New(sandbox.Policy{Workspace: workspace, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 10 * time.Second})
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
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"printf shell-ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(string(result.Output), "shell-ok") {
		t.Fatalf("result = %#v", result)
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
