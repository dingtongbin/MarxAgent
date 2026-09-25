// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

type process struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	wait   func() (Result, error)
	close  func() error
	once   sync.Once
	mu     sync.Mutex
	result Result
	err    error
	waited bool
}

func newProcess(stdin io.WriteCloser, stdout, stderr io.ReadCloser, wait func() (Result, error), close func() error) Process {
	if close == nil {
		close = func() error { return nil }
	}
	return &process{stdin: stdin, stdout: stdout, stderr: stderr, wait: wait, close: close}
}

func (p *process) Stdin() io.WriteCloser {
	return p.stdin
}

func (p *process) Stdout() io.ReadCloser {
	return p.stdout
}

func (p *process) Stderr() io.ReadCloser {
	return p.stderr
}

func (p *process) Wait() (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.waited || p.wait == nil {
		return p.result, p.err
	}
	p.waited = true
	p.result, p.err = p.wait()
	return p.result, p.err
}

func (p *process) Close() error {
	var err error
	p.once.Do(func() {
		p.mu.Lock()
		p.mu.Unlock()
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.stdout != nil {
			_ = p.stdout.Close()
		}
		if p.stderr != nil {
			_ = p.stderr.Close()
		}
		err = errorsJoin(err, p.close())
	})
	return err
}

type cancelProcess struct {
	Process
	cancel context.CancelFunc
}

func (p *cancelProcess) Close() error {
	p.cancel()
	return p.Process.Close()
}

func startDirect(ctx context.Context, argv []string, env []string, dir string) (Process, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%w: command is required", ErrInvalidPolicy)
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = dir
	if env != nil {
		command.Env = append([]string(nil), env...)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return newProcess(stdin, stdout, stderr, func() (Result, error) {
		err := command.Wait()
		code := command.ProcessState.ExitCode()
		return Result{ExitCode: code}, err
	}, func() error {
		if command.Process != nil && command.ProcessState == nil {
			return command.Process.Kill()
		}
		return nil
	}), nil
}

func Run(ctx context.Context, runner Runner, argv []string, env []string, dir string, input io.Reader, output, errorOutput io.Writer) (Result, error) {
	if runner == nil {
		return Result{}, ErrRequired
	}
	process, err := runner.Start(ctx, argv, env, dir)
	if err != nil {
		return Result{}, err
	}
	if output == nil {
		output = io.Discard
	}
	if errorOutput == nil {
		errorOutput = io.Discard
	}
	var waitGroup sync.WaitGroup
	if input != nil && process.Stdin() != nil {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, _ = io.Copy(process.Stdin(), input)
			_ = process.Stdin().Close()
		}()
	} else if process.Stdin() != nil {
		_ = process.Stdin().Close()
	}
	if output != nil && process.Stdout() != nil {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, _ = io.Copy(output, process.Stdout())
		}()
	}
	if errorOutput != nil && process.Stderr() != nil {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, _ = io.Copy(errorOutput, process.Stderr())
		}()
	}
	result, waitErr := process.Wait()
	waitGroup.Wait()
	closeErr := process.Close()
	if waitErr != nil {
		return result, waitErr
	}
	return result, closeErr
}

func errorsJoin(left, right error) error {
	return errors.Join(left, right)
}
