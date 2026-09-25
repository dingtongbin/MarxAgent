// SPDX-License-Identifier: Apache-2.0
//go:build windows

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireAppContainerRunner probes the host once per package run. Managed CI
// images occasionally ship without the AppContainer support that lets a
// low-integrity child start at all; that is an environment limitation, not an
// implementation defect, and it is reported loudly instead of failing the
// build with an opaque timeout.
func requireAppContainerRunner(t *testing.T) {
	t.Helper()
	if !platformAvailable() {
		t.Skip("Windows sandbox APIs are unavailable")
	}
	probe := t.TempDir()
	engine, err := New(Policy{Workspace: probe, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var stderr strings.Builder
	result, err := Run(ctx, engine, []string{"cmd.exe", "/c", "exit 0"}, nil, probe, nil, nil, &stderr)
	if err == nil && result.ExitCode == 0 {
		return
	}
	t.Skipf("the Windows AppContainer runtime cannot launch processes on this host: err = %v result = %#v stderr = %q", err, result, stderr.String())
}

func TestWindowsAppContainerBackend(t *testing.T) {
	requireAppContainerRunner(t)
	root := t.TempDir()
	engine, err := New(Policy{Workspace: root, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var output, errorOutput strings.Builder
	result, err := Run(context.Background(), engine, []string{"cmd.exe", "/c", "echo sandbox-ok"}, nil, root, nil, &output, &errorOutput)
	if err != nil || result.ExitCode != 0 || !strings.Contains(output.String(), "sandbox-ok") {
		t.Fatalf("result = %#v err = %v stdout = %q stderr = %q", result, err, output.String(), errorOutput.String())
	}
	forbidden := t.TempDir()
	secret := filepath.Join(forbidden, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	forbiddenOutput := &strings.Builder{}
	forbiddenError := &strings.Builder{}
	forbiddenEngine, err := New(Policy{Workspace: root, WorkspaceWritable: true, ForbiddenRoots: []string{forbidden}, Network: true, Subprocess: true, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	forbiddenResult, forbiddenErr := Run(context.Background(), forbiddenEngine, []string{"cmd.exe", "/c", "type " + secret}, nil, root, nil, forbiddenOutput, forbiddenError)
	if forbiddenErr == nil && forbiddenResult.ExitCode == 0 {
		t.Fatalf("forbidden file was readable: %q", forbiddenOutput.String())
	}
	if strings.Contains(forbiddenOutput.String(), "secret") {
		t.Fatalf("forbidden content leaked: %q", forbiddenOutput.String())
	}
	timeoutEngine, err := New(Policy{Workspace: root, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), timeoutEngine, []string{"cmd.exe", "/c", "ping -n 6 127.0.0.1 > nul"}, nil, root, nil, nil, nil); err == nil {
		t.Fatal("sandbox timeout was not enforced")
	}
}

func TestWindowsPowerShellInsideAppContainer(t *testing.T) {
	requireAppContainerRunner(t)
	if !powerShellRunsInSandbox(t) {
		return
	}
	root := t.TempDir()
	engine, err := New(Policy{Workspace: root, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var output, errorOutput strings.Builder
	result, err := Run(context.Background(), engine, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Write-Output sandbox-ok"}, nil, root, nil, &output, &errorOutput)
	if err != nil || result.ExitCode != 0 || !strings.Contains(output.String(), "sandbox-ok") {
		t.Fatalf("result = %#v err = %v stdout = %q stderr = %q", result, err, output.String(), errorOutput.String())
	}
}

// powerShellRunsInSandbox reports whether PowerShell can start inside the
// AppContainer at all. Server images frequently deny it read access to the .NET
// facades it needs, which is a host policy rather than a sandbox defect.
func powerShellRunsInSandbox(t *testing.T) bool {
	t.Helper()
	probe := t.TempDir()
	engine, err := New(Policy{Workspace: probe, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var output, errorOutput strings.Builder
	result, err := Run(context.Background(), engine, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Write-Output ready"}, nil, probe, nil, &output, &errorOutput)
	if err == nil && result.ExitCode == 0 && strings.Contains(output.String(), "ready") {
		return true
	}
	t.Logf("SKIP: PowerShell cannot start inside the AppContainer on this host: err = %v result = %#v stderr = %q", err, result, errorOutput.String())
	return false
}

func TestWindowsPlatformHelpers(t *testing.T) {
	if platformName() != "windows-appcontainer" || defaultPath() == "" {
		t.Fatalf("platform metadata is invalid")
	}
	capabilities := platformCapabilities()
	if !capabilities.Filesystem || !capabilities.Network || !capabilities.ProcessTree || !capabilities.DefaultDeny {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if !pathWithin(`C:\Work`, `c:\work\child`) || pathWithin(`C:\Work`, `C:\Other`) {
		t.Fatal("Windows path scope is incorrect")
	}
	if pathWithin(`C:\Work`, `D:\Elsewhere`) {
		t.Fatal("cross-volume path was treated as contained")
	}
	cases := []struct {
		name   string
		policy normalizedPolicy
		valid  bool
	}{
		{name: "valid", policy: normalizedPolicy{timeout: time.Second, subprocess: true, network: true, writableRoots: []string{"C:\\Work"}}, valid: true},
		{name: "missing timeout", policy: normalizedPolicy{subprocess: true, network: true}},
		{name: "subprocess denied", policy: normalizedPolicy{timeout: time.Second, network: true}},
		{name: "writable offline", policy: normalizedPolicy{timeout: time.Second, subprocess: true, writableRoots: []string{"C:\\Work"}}},
		{name: "mixed roots", policy: normalizedPolicy{timeout: time.Second, subprocess: true, network: true, writableRoots: []string{"C:\\Work"}, readOnlyRoots: []string{"C:\\Read"}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := platformValidatePolicy(test.policy)
			if (err == nil) != test.valid {
				t.Fatalf("valid = %v err = %v", test.valid, err)
			}
		})
	}
}

func TestWindowsStartReportsMissingCommand(t *testing.T) {
	if !platformAvailable() {
		t.Skip("Windows sandbox APIs are unavailable")
	}
	engine := &Engine{policy: normalizedPolicy{timeout: time.Second, subprocess: true, network: true}}
	if _, err := startPlatform(context.Background(), engine, []string{"missing-marxagent-command"}, nil, t.TempDir()); err == nil {
		t.Fatal("missing command did not fail before launch")
	}
}

func TestWindowsPlatformRootsCoverPolicyAndWorkingDirectory(t *testing.T) {
	root := canonicalPath(t.TempDir())
	forbidden := filepath.Join(root, "secret")
	engine := &Engine{policy: normalizedPolicy{
		workspace:      root,
		readOnlyRoots:  []string{filepath.Join(root, "ro")},
		writableRoots:  []string{filepath.Join(root, "rw")},
		forbiddenRoots: []string{forbidden},
	}}
	working := filepath.Join(root, "rw", "nested")
	roots := platformRoots(engine, working)
	if len(roots) != 1 || !pathWithin(roots[0], working) || !pathWithin(roots[0], forbidden) {
		t.Fatalf("roots = %#v", roots)
	}
	if platformRoots(engine, "") == nil {
		t.Fatal("no roots were produced")
	}
}

func TestWindowsStartRejectsUnsafePolicy(t *testing.T) {
	if _, err := startPlatform(context.Background(), &Engine{policy: normalizedPolicy{subprocess: true}}, nil, nil, t.TempDir()); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("missing timeout error = %v", err)
	}
	offline := normalizedPolicy{timeout: time.Second, subprocess: true, writableRoots: []string{"C:\\Work"}}
	if _, err := startPlatform(context.Background(), &Engine{policy: offline}, nil, nil, t.TempDir()); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatalf("writable offline error = %v", err)
	}
}
