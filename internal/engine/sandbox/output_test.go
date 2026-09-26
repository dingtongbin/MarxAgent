// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Cmd.StdoutPipe closes the reading end as soon as Wait sees the process exit, so
// whatever the process wrote and nobody had read yet is discarded. A process that
// answers and exits promptly is exactly the case that loses its output, and the
// symptom is a clean exit code with nothing in it, which reads like a command that
// printed nothing rather than like a lost race.
//
// This is written against os/exec rather than against a platform backend so it runs
// everywhere, and so a backend that reaches for the pipe helpers again is caught on
// the platform where nobody is watching rather than on the one that only fails
// under load.
func TestOutputIsNotLostWhenAProcessExitsPromptly(t *testing.T) {
	// The two forms are spelled the way each platform spells them, because the
	// claim is about os/exec and not about any one shell.
	shell, flag, output := hostShell()
	// The lost form: ask exec for a pipe, then wait, then read what is left.
	lost := exec.Command(shell, flag, output)
	lostPipe, err := lost.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = lost.Start()
	_ = lost.Wait()
	remaining, _ := io.ReadAll(lostPipe)
	t.Logf("reading after Wait gave %q", string(remaining))

	// The kept form, which is what the backends do: hold the reading end, hand the
	// writing end to the process, and let a reader drain it before the process is
	// waited for.
	kept := exec.Command(shell, flag, output)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	kept.Stdout = writer
	if err := kept.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	drained := make(chan string, 1)
	go func() {
		content, _ := io.ReadAll(reader)
		drained <- string(content)
	}()
	_ = kept.Wait()
	select {
	case text := <-drained:
		if !strings.Contains(text, sandboxCanary) {
			t.Fatalf("drained %q, want the output", text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reader was never finished")
	}
	_ = reader.Close()
}

// hostShell is the command this host uses to print one line.
func hostShell() (name string, flag string, script string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/c", "echo " + sandboxCanary
	}
	return "sh", "-c", "printf " + sandboxCanary
}

const sandboxCanary = "sandbox-canary-7c1f"
