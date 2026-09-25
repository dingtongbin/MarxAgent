// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type sandboxTransport struct {
	runner sandbox.Runner
	config ClientConfig
}

func (t *sandboxTransport) Connect(ctx context.Context) (mcpsdk.Connection, error) {
	if t == nil || t.runner == nil {
		return nil, sandbox.ErrRequired
	}
	argv := append([]string{t.config.Command}, t.config.Args...)
	environment := append(os.Environ(), t.config.Env...)
	process, err := t.runner.Start(ctx, argv, environment, t.config.Dir)
	if err != nil {
		return nil, err
	}
	if process.Stdin() == nil || process.Stdout() == nil {
		_ = process.Close()
		return nil, fmt.Errorf("mcp: sandbox process is missing stdio")
	}
	if process.Stderr() != nil {
		go func() {
			_, _ = io.Copy(io.Discard, process.Stderr())
		}()
	}
	return (&mcpsdk.IOTransport{
		Reader: &sandboxReader{ReadCloser: process.Stdout(), process: process},
		Writer: &sandboxWriter{WriteCloser: process.Stdin(), process: process},
	}).Connect(ctx)
}

type sandboxReader struct {
	io.ReadCloser
	process sandbox.Process
}

func (r *sandboxReader) Close() error {
	return errors.Join(r.ReadCloser.Close(), r.process.Close())
}

type sandboxWriter struct {
	io.WriteCloser
	process sandbox.Process
}

func (w *sandboxWriter) Close() error {
	return errors.Join(w.WriteCloser.Close(), w.process.Close())
}
