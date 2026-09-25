// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
)

const defaultShellOutputBytes = 1 << 20

type ShellConfig struct {
	Shell            string
	WorkingDirectory string
	Env              []string
	MaxOutputBytes   int
	Sandbox          sandbox.Runner
}

type shellTool struct {
	config ShellConfig
}

type shellParams struct {
	Command string `json:"command"`
}

var shellSchema = json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","minLength":1}},"required":["command"],"additionalProperties":false}`)

func NewShellTool(config ShellConfig) (core.Tool, error) {
	if config.Sandbox == nil {
		return nil, sandbox.ErrRequired
	}
	if config.WorkingDirectory != "" {
		info, err := os.Stat(config.WorkingDirectory)
		if err != nil {
			return nil, fmt.Errorf("tools: shell working directory: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("tools: shell working directory is not a directory")
		}
	}
	if strings.IndexByte(config.Shell, 0) >= 0 || strings.IndexByte(config.WorkingDirectory, 0) >= 0 {
		return nil, fmt.Errorf("tools: shell configuration contains NUL")
	}
	for _, value := range config.Env {
		if strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("tools: shell environment contains NUL")
		}
	}
	if config.MaxOutputBytes < 0 {
		return nil, fmt.Errorf("tools: shell max output bytes must not be negative")
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = defaultShellOutputBytes
	}
	config.Env = append([]string(nil), config.Env...)
	return &shellTool{config: config}, nil
}

func (t *shellTool) Name() string {
	return "bash"
}

func (t *shellTool) Description() string {
	return "Execute a shell command in the configured working directory."
}

func (t *shellTool) Parameters() json.RawMessage {
	return append(json.RawMessage(nil), shellSchema...)
}

func (t *shellTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	if t == nil {
		return core.ToolResult{}, fmt.Errorf("tools: shell tool is not initialized")
	}
	if err := contextError(ctx); err != nil {
		return core.ToolResult{}, err
	}
	var params shellParams
	if err := decodeParams(raw, &params); err != nil {
		return core.ToolResult{}, err
	}
	if strings.TrimSpace(params.Command) == "" {
		return core.ToolResult{}, fmt.Errorf("tools: shell command must not be empty")
	}
	argv, err := shellArgs(t.config, params.Command)
	if err != nil {
		return core.ToolResult{}, err
	}
	environment := append(os.Environ(), t.config.Env...)
	stdout := &cappedBuffer{limit: t.config.MaxOutputBytes}
	stderr := &cappedBuffer{limit: t.config.MaxOutputBytes}
	runResult, runErr := sandbox.Run(ctx, t.config.Sandbox, argv, environment, t.config.WorkingDirectory, nil, stdout, stderr)
	if runErr != nil && runResult.ExitCode <= 0 {
		var exitError *exec.ExitError
		if !errors.As(runErr, &exitError) {
			return core.ToolResult{}, fmt.Errorf("tools: execute shell command: %w", runErr)
		}
	}
	exitCode := runResult.ExitCode
	isError := exitCode != 0
	output, err := json.Marshal(struct {
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		ExitCode  int    `json:"exit_code"`
		Truncated bool   `json:"truncated"`
	}{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitCode, Truncated: stdout.truncated || stderr.truncated})
	if err != nil {
		return core.ToolResult{}, err
	}
	result := core.ToolResult{Output: output, IsError: isError}
	if isError {
		result.Error = fmt.Sprintf("shell command exited with status %d", exitCode)
	}
	return result, nil
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(data) > remaining {
		b.Buffer.Write(data[:remaining])
		b.truncated = true
		return original, nil
	}
	b.Buffer.Write(data)
	return original, nil
}
