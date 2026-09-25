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

func TestWindowsAppContainerBackend(t *testing.T) {
	if !platformAvailable() {
		t.Skip("Windows sandbox APIs are unavailable")
	}
	root := t.TempDir()
	engine, err := New(Policy{Workspace: root, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	result, err := Run(context.Background(), engine, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Write-Output sandbox-ok"}, nil, root, nil, &output, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(output.String(), "sandbox-ok") {
		t.Fatalf("result = %#v output = %q", result, output.String())
	}
	forbidden := t.TempDir()
	secret := filepath.Join(forbidden, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	forbiddenEngine, err := New(Policy{Workspace: root, WorkspaceWritable: true, ForbiddenRoots: []string{forbidden}, Network: true, Subprocess: true, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	forbiddenOutput := &strings.Builder{}
	forbiddenResult, forbiddenErr := Run(context.Background(), forbiddenEngine, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Get-Content -Raw " + secret}, nil, root, nil, forbiddenOutput, nil)
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
	if _, err := Run(context.Background(), timeoutEngine, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 5"}, nil, root, nil, nil, nil); err == nil {
		t.Fatal("sandbox timeout was not enforced")
	}
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
	process, err := startPlatform(context.Background(), engine, []string{"missing-marxagent-command"}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("start failed early: %v", err)
	}
	defer process.Close()
	if _, err := process.Wait(); err == nil {
		t.Fatal("missing command did not fail")
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
