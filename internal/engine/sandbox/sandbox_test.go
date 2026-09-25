// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPolicyRequiresWorkspaceAndRejectsUnsafeValues(t *testing.T) {
	if _, err := New(Policy{}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("empty policy error = %v", err)
	}
	root := t.TempDir()
	if _, err := New(Policy{Workspace: root, AllowLoopback: true}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("loopback policy error = %v", err)
	}
	if _, err := New(Policy{Workspace: root, Timeout: -time.Second}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("timeout policy error = %v", err)
	}
	if _, err := New(Policy{Workspace: root, ReadOnlyRoots: []string{"bad\x00path"}}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("NUL policy error = %v", err)
	}
}

func TestPolicyCopiesAndFiltersEnvironment(t *testing.T) {
	root := t.TempDir()
	readRoot := t.TempDir()
	engine, err := New(Policy{Workspace: root, ReadOnlyRoots: []string{readRoot}, Network: true, Subprocess: true, Timeout: 10 * time.Second, EnvAllowlist: []string{"PATH", "CUSTOM"}})
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if engine.Capabilities().Backend == "" {
		t.Fatal("sandbox backend is empty")
	}
	policy := engine.Policy()
	if len(policy.ReadOnlyRoots) == 0 {
		t.Fatal("read-only root was not normalized")
	}
	policy.ReadOnlyRoots[0] = "mutated"
	if engine.Policy().ReadOnlyRoots[0] == "mutated" {
		t.Fatal("policy was not copied")
	}
	if !engine.policy.allowsPath(readRoot) || engine.policy.allowsPath(filepath.Join(root, "..", "outside")) {
		t.Fatal("policy path scope is incorrect")
	}
	filtered := engine.filterEnvironment([]string{"PATH=/bin", "SECRET=hidden", "CUSTOM=value"})
	if strings.Contains(strings.Join(filtered, "\n"), "SECRET") {
		t.Fatalf("secret survived environment filtering: %v", filtered)
	}
	if !strings.Contains(strings.Join(filtered, "\n"), "CUSTOM=value") {
		t.Fatalf("allowlisted variable missing: %v", filtered)
	}
}

func TestUnrestrictedRunnerCopiesStdioAndPropagatesExit(t *testing.T) {
	engine := NewUnrestricted()
	if engine == nil {
		t.Fatal("nil unrestricted engine")
	}
	var stdout, stderr strings.Builder
	result, err := Run(context.Background(), engine, []string{os.Args[0], "-test.run=TestSandboxHelperProcess", "--"}, []string{"MARXAGENT_SANDBOX_HELPER=copy"}, t.TempDir(), strings.NewReader("input"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || stdout.String() != "output:input" || !strings.Contains(stderr.String(), "error") {
		t.Fatalf("result = %#v stdout = %q stderr = %q", result, stdout.String(), stderr.String())
	}
}

func TestUnrestrictedRunnerCancelsAndReportsStartErrors(t *testing.T) {
	engine := NewUnrestricted()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, engine, []string{os.Args[0], "-test.run=TestSandboxHelperProcess", "--"}, []string{"MARXAGENT_SANDBOX_HELPER=sleep"}, t.TempDir(), nil, nil, nil); err == nil {
		t.Fatal("cancellation was not reported")
	}
	if _, err := engine.Start(context.Background(), []string{"missing-marxagent-direct-command"}, nil, ""); err == nil {
		t.Fatal("missing direct command was accepted")
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := engine.Start(canceled, []string{os.Args[0]}, nil, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled start error = %v", err)
	}
	if _, err := engine.Start(context.Background(), nil, nil, ""); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("empty argv error = %v", err)
	}
	if _, err := engine.Start(nil, []string{"cmd"}, nil, ""); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("nil context error = %v", err)
	}
	var nilEngine *Engine
	if _, err := nilEngine.Start(context.Background(), []string{"cmd"}, nil, ""); !errors.Is(err, ErrRequired) {
		t.Fatalf("nil engine error = %v", err)
	}
}

func TestSandboxHelperProcess(t *testing.T) {
	switch os.Getenv("MARXAGENT_SANDBOX_HELPER") {
	case "copy":
		data, _ := io.ReadAll(os.Stdin)
		_, _ = os.Stdout.WriteString("output:" + string(data))
		_, _ = os.Stderr.WriteString("error")
		os.Exit(0)
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
}

func TestNormalizeRootsRejectsMissingWritableRoot(t *testing.T) {
	root := t.TempDir()
	_, err := New(Policy{Workspace: root, WritableRoots: []string{filepath.Join(root, "missing")}})
	if !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("missing writable root error = %v", err)
	}
}

func TestResolveExecutableAndNormalizationEdges(t *testing.T) {
	executable, err := resolveExecutable(os.Args[0])
	if err != nil || !filepath.IsAbs(executable) {
		t.Fatalf("executable = %q err = %v", executable, err)
	}
	if _, err := resolveExecutable(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing executable accepted")
	}
	if _, err := resolveExecutable(t.TempDir()); err == nil {
		t.Fatal("directory executable accepted")
	}
	if _, err := resolveExecutable("bad\x00path"); err == nil {
		t.Fatal("NUL executable accepted")
	}
	link := filepath.Join(t.TempDir(), "linked-exe")
	if err := os.Symlink(os.Args[0], link); err == nil {
		if resolved, err := resolveExecutable(link); err != nil || !filepath.IsAbs(resolved) {
			t.Fatalf("symlink resolution = %q err = %v", resolved, err)
		}
	}
	t.Setenv("PATH", filepath.Dir(os.Args[0])+string(os.PathListSeparator)+os.Getenv("PATH"))
	if resolved, err := resolveExecutable(filepath.Base(os.Args[0])); err != nil || !filepath.IsAbs(resolved) {
		t.Fatalf("PATH lookup = %q err = %v", resolved, err)
	}
	if _, err := normalizeDirectory("", false); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeDirectory("bad\x00path", false); err == nil {
		t.Fatal("NUL directory accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeDirectory(file, true); err == nil {
		t.Fatal("file directory accepted")
	}
	roots, err := normalizeRoots([]string{file, file}, false)
	if err != nil || len(roots) != 1 {
		t.Fatalf("roots = %#v err = %v", roots, err)
	}
	if _, err := normalizeRoots([]string{""}, false); err == nil {
		t.Fatal("empty root accepted")
	}
	root := t.TempDir()
	if got := addRoot([]string{root}, root); len(got) != 1 {
		t.Fatalf("duplicate root = %#v", got)
	}
}

func TestPolicyEngineRejectsUnsafeWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	engine, err := New(Policy{Workspace: root, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 10 * time.Second})
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	outside := t.TempDir()
	if _, err := engine.Start(context.Background(), []string{"cmd"}, nil, outside); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("outside working directory error = %v", err)
	}
	if _, err := engine.Start(context.Background(), []string{"cmd", "bad\x00arg"}, nil, root); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("NUL argument error = %v", err)
	}
}

func TestRunRejectsNilRunner(t *testing.T) {
	if _, err := Run(context.Background(), nil, []string{"cmd"}, nil, "", nil, nil, nil); !errors.Is(err, ErrRequired) {
		t.Fatalf("nil runner error = %v", err)
	}
}

type stubRunner struct {
	closeErr error
}

func (r stubRunner) Start(context.Context, []string, []string, string) (Process, error) {
	wait := func() (Result, error) { return Result{ExitCode: 3}, nil }
	return newProcess(nil, io.NopCloser(strings.NewReader("out")), io.NopCloser(strings.NewReader("err")), wait, func() error {
		return r.closeErr
	}), nil
}

func (stubRunner) Capabilities() Capabilities {
	return Capabilities{Backend: "stub", Filesystem: true, DefaultDeny: true}
}

func TestRunPropagatesCloseErrorAndDiscardsUnreadStreams(t *testing.T) {
	closeErr := errors.New("close failed")
	result, err := Run(context.Background(), stubRunner{closeErr: closeErr}, []string{"cmd"}, nil, "", strings.NewReader("input"), nil, nil)
	if !errors.Is(err, closeErr) {
		t.Fatalf("close error = %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("result = %#v", result)
	}
}

func TestFilterEnvironmentKeepsAllowlistAndDefaultsPath(t *testing.T) {
	engine := &Engine{policy: normalizedPolicy{envAllowlist: []string{"A", "PATH"}}}
	filtered := engine.filterEnvironment([]string{"A=1", "B=2", "malformed", "=novalue"})
	if len(filtered) != 2 || filtered[0] != "A=1" || filtered[1] != "PATH="+defaultPath() {
		t.Fatalf("filtered = %#v", filtered)
	}
}

func TestStartDirectInheritsEnvironmentWhenUnset(t *testing.T) {
	engine := NewUnrestricted()
	process, err := engine.Start(context.Background(), []string{os.Args[0], "-test.run=TestSandboxHelperProcess", "--", "exit:0"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer process.Close()
	if _, err := process.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestNilEngineAccessors(t *testing.T) {
	var engine *Engine
	if engine.Capabilities().Backend != "" {
		t.Fatal("nil engine returned capabilities")
	}
	if engine.Policy().Workspace != "" {
		t.Fatal("nil engine returned a policy")
	}
	if _, err := engine.Start(context.Background(), []string{"cmd"}, nil, ""); !errors.Is(err, ErrRequired) {
		t.Fatalf("nil engine start error = %v", err)
	}
}

func TestAllowsPathCoversReadOnlyAndWritableRoots(t *testing.T) {
	root := t.TempDir()
	readOnly := filepath.Join(root, "ro")
	writable := filepath.Join(root, "rw")
	if err := os.MkdirAll(readOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(writable, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := normalizedPolicy{workspace: root, readOnlyRoots: []string{readOnly}, writableRoots: []string{writable}}
	if !policy.allowsPath(readOnly) || !policy.allowsPath(writable) || !policy.allowsPath(root) {
		t.Fatal("declared roots were rejected")
	}
	if policy.allowsPath(t.TempDir()) {
		t.Fatal("unrelated path was accepted")
	}
	if (normalizedPolicy{}).allowsPath(root) {
		t.Fatal("empty policy accepted a path")
	}
}

func TestNormalizeDirectoryResolvesMissingParents(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent", "child")
	resolved, err := normalizeDirectory(missing, false)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("resolved path is not absolute: %q", resolved)
	}
}

func TestNormalizePolicyRejectsUnusableRoots(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	cases := []Policy{
		{Workspace: root, ReadOnlyRoots: []string{"bad\x00root"}},
		{Workspace: root, WritableRoots: []string{missing}},
		{Workspace: root, ForbiddenRoots: []string{missing}},
	}
	for _, policy := range cases {
		if _, err := normalizePolicy(policy); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("normalizePolicy(%#v) error = %v", policy, err)
		}
	}
}

func TestStartSurfacesPlatformStartFailure(t *testing.T) {
	root := t.TempDir()
	engine := &Engine{policy: normalizedPolicy{workspace: root, subprocess: true}}
	process, err := engine.Start(context.Background(), []string{"cmd"}, nil, root)
	if err == nil {
		_ = process.Close()
		t.Fatal("unsupported policy was accepted by the platform backend")
	}
}

func TestNewSurfacesPlatformPolicyRejection(t *testing.T) {
	root := t.TempDir()
	if _, err := New(Policy{Workspace: root, AllowLoopback: true, Subprocess: true, Timeout: time.Second}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("loopback without network error = %v", err)
	}
	if _, err := New(Policy{Workspace: root, Subprocess: true, Timeout: -time.Second}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("negative timeout error = %v", err)
	}
	// Only the AppContainer backend needs a positive timeout and an offline
	// writable sandbox; the Unix backends accept the same policy.
	if platformName() != "windows-appcontainer" {
		return
	}
	if _, err := New(Policy{Workspace: root, Subprocess: true}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("platform policy rejection error = %v", err)
	}
}

func TestProcessLifecycleWithoutStreams(t *testing.T) {
	waited := false
	process := newProcess(nil, nil, nil, func() (Result, error) {
		waited = true
		return Result{ExitCode: 0}, nil
	}, nil)
	if process.Stdin() != nil || process.Stdout() != nil || process.Stderr() != nil {
		t.Fatal("unexpected process streams")
	}
	if result, err := process.Wait(); err != nil || result.ExitCode != 0 || !waited {
		t.Fatalf("wait result = %#v err = %v waited = %v", result, err, waited)
	}
	if result, err := process.Wait(); err != nil || result.ExitCode != 0 {
		t.Fatalf("second wait result = %#v err = %v", result, err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMorePolicyEdges(t *testing.T) {
	root := t.TempDir()
	if _, err := New(Policy{Workspace: root, ForbiddenRoots: []string{root}, Subprocess: true, Timeout: time.Second}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("workspace forbidden error = %v", err)
	}
	if _, err := normalizeDirectory(filepath.Join(root, "missing"), true); err == nil {
		t.Fatal("missing directory accepted")
	}
	values := normalizeEnvAllowlist([]string{"A", "A", "=", "bad\x00key", "B"})
	if len(values) != 2 || values[0] != "A" || values[1] != "B" {
		t.Fatalf("allowlist = %#v", values)
	}
	engine, err := New(Policy{Workspace: root, Subprocess: true, Timeout: time.Second})
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if filtered := engine.filterEnvironment(nil); len(filtered) == 0 {
		t.Fatal("default environment was empty")
	}
	if engine.Policy().WorkspaceWritable {
		t.Fatal("unexpected workspace writability")
	}
}
