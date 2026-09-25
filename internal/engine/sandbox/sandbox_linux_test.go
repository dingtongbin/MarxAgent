// SPDX-License-Identifier: Apache-2.0
//go:build linux

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

func TestLinuxBubblewrapBackend(t *testing.T) {
	if !platformAvailable() {
		t.Skip("bubblewrap is unavailable")
	}
	root := t.TempDir()
	engine, err := New(Policy{Workspace: root, WorkspaceWritable: true, Network: true, Subprocess: true, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	result, err := Run(context.Background(), engine, []string{"/bin/sh", "-c", "printf sandbox-ok"}, nil, root, nil, &output, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || output.String() != "sandbox-ok" {
		t.Fatalf("result = %#v output = %q", result, output.String())
	}
}

func TestLinuxBwrapArgumentsAreClosedAndNetworkDenied(t *testing.T) {
	if !platformAvailable() {
		t.Skip("bubblewrap is unavailable")
	}
	root := t.TempDir()
	temp := t.TempDir()
	engine, err := New(Policy{Workspace: root, WorkspaceWritable: true, Subprocess: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	args, err := buildBwrapArgs(engine, "/bin/sh", []string{"/bin/sh", "-c", "printf ok; touch /tmp/x"}, temp, root, []string{"PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	for _, required := range []string{"--unshare-net", "--clearenv", "--unshare-pid", "--die-with-parent", "--"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing bwrap argument %q in %v", required, args)
		}
	}
	if strings.Contains(joined, "/home") || strings.Contains(joined, "/root") {
		t.Fatalf("host home was exposed: %v", args)
	}
	if args[len(args)-2] != "-c" || args[len(args)-1] != "printf ok; touch /tmp/x" {
		t.Fatalf("argv was not passed as data: %v", args)
	}
}

func TestLinuxBwrapArgumentsMaskForbiddenFilesAndDeduplicate(t *testing.T) {
	root := t.TempDir()
	secretFile := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{policy: normalizedPolicy{
		workspace:      root,
		writableRoots:  []string{root, root},
		readOnlyRoots:  []string{filepath.Join(root, "missing")},
		forbiddenRoots: []string{secretFile, filepath.Join(root, "missing-dir")},
		subprocess:     true,
		network:        true,
	}}
	args, err := buildBwrapArgs(engine, "/bin/sh", []string{"/bin/sh"}, filepath.Join(root, "temp"), root, []string{"PATH=/bin", "INVALID"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, os.DevNull+" "+secretFile) {
		t.Fatalf("forbidden file was not masked: %v", args)
	}
	if strings.Contains(joined, "missing-dir") {
		t.Fatalf("missing forbidden root was bound: %v", args)
	}
	if strings.Contains(joined, "INVALID") {
		t.Fatalf("malformed environment entry was exported: %v", args)
	}
	binds := 0
	for index, arg := range args {
		if arg == "--bind" && index+1 < len(args) && args[index+1] == root {
			binds++
		}
	}
	if binds != 1 {
		t.Fatalf("workspace was bound %d times: %v", binds, args)
	}
}

func TestLinuxPlatformHelpers(t *testing.T) {
	if platformName() != "bubblewrap" || defaultPath() != "/usr/bin:/bin" {
		t.Fatal("unexpected Linux backend metadata")
	}
	capabilities := platformCapabilities()
	if !capabilities.Filesystem || !capabilities.Network || !capabilities.ProcessTree || !capabilities.DefaultDeny {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if !pathWithin("/usr", "/usr/bin/sh") || pathWithin("/usr", "/var/lib") {
		t.Fatal("Linux path scope is incorrect")
	}
	if trustedBackendPath("/tmp/bwrap") || !trustedBackendPath("/usr/bin/bwrap") {
		t.Fatal("backend path trust check is incorrect")
	}
	if err := platformValidatePolicy(normalizedPolicy{subprocess: false}); err == nil {
		t.Fatal("subprocess restriction was silently ignored")
	}
	if err := platformValidatePolicy(normalizedPolicy{subprocess: true}); err != nil {
		t.Fatal(err)
	}
	if len(linuxSystemReadRoots()) == 0 || !pathVisible("/bin/sh", map[string]struct{}{"/bin": {}}) {
		t.Fatal("system path visibility is incorrect")
	}
}

func TestLinuxStartReportsMissingCommand(t *testing.T) {
	if !platformAvailable() {
		t.Skip("bubblewrap is unavailable")
	}
	engine := &Engine{policy: normalizedPolicy{timeout: time.Second, subprocess: true, network: true}}
	if _, err := startPlatform(context.Background(), engine, []string{"missing-marxagent-command"}, nil, t.TempDir()); err == nil {
		t.Fatal("missing command did not fail")
	}
}

func TestLinuxBubblewrapHidesForbiddenRoot(t *testing.T) {
	if !platformAvailable() {
		t.Skip("bubblewrap is unavailable")
	}
	root := t.TempDir()
	forbidden := t.TempDir()
	secret := forbidden + "/secret"
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	engine, err := New(Policy{Workspace: root, WorkspaceWritable: true, ForbiddenRoots: []string{forbidden}, Network: true, Subprocess: true, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), engine, []string{"/bin/sh", "-c", "cat " + secret}, nil, root, nil, nil, nil)
	if err == nil {
		t.Fatal("forbidden file was readable")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}
