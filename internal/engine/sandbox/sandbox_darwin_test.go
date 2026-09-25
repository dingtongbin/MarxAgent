// SPDX-License-Identifier: Apache-2.0
//go:build darwin

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDarwinProfileAndHelpers(t *testing.T) {
	if platformName() != "seatbelt" || defaultPath() != "/usr/bin:/bin" {
		t.Fatal("unexpected Darwin backend metadata")
	}
	capabilities := platformCapabilities()
	if !capabilities.Filesystem || !capabilities.Network || !capabilities.ProcessTree || !capabilities.DefaultDeny {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if !pathWithin("/usr", "/usr/bin/sh") || pathWithin("/usr", "/var/lib") {
		t.Fatal("Darwin path scope is incorrect")
	}
	if !trustedBackendPath("/usr/bin/sandbox-exec") || trustedBackendPath("/tmp/sandbox-exec") {
		t.Fatal("backend path trust check is incorrect")
	}
	if err := platformValidatePolicy(normalizedPolicy{}); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	forbidden := t.TempDir()
	engine := &Engine{policy: normalizedPolicy{workspace: root, writableRoots: []string{root}, forbiddenRoots: []string{forbidden}, subprocess: true}}
	profile, err := buildSBPL(engine, "/bin/sh", root+"/tmp")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"(deny default)", "(allow file-write*", "(deny file-read*", "(allow file-read*"} {
		if !strings.Contains(profile, required) {
			t.Fatalf("profile missing %q: %s", required, profile)
		}
	}
	if strings.Contains(profile, "(allow network*)") {
		t.Fatalf("offline profile allowed network: %s", profile)
	}
	online := &Engine{policy: normalizedPolicy{workspace: root, writableRoots: []string{root}, forbiddenRoots: []string{forbidden}, subprocess: true, network: true}}
	onlineProfile, err := buildSBPL(online, "/bin/sh", root+"/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(onlineProfile, "(allow network*)") {
		t.Fatalf("online profile denied network: %s", onlineProfile)
	}
	noFork := &Engine{policy: normalizedPolicy{workspace: root, subprocess: false}}
	noForkProfile, err := buildSBPL(noFork, "/bin/sh", root+"/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(noForkProfile, "(deny process-fork)") || !strings.Contains(noForkProfile, "(allow process-exec*") {
		t.Fatalf("no-fork profile is incorrect: %s", noForkProfile)
	}
	if _, err := buildSBPL(engine, "/bin/sh", "bad\"); (allow default)"); err == nil {
		t.Fatal("Seatbelt literal injection was accepted")
	}
	if err := validateSBPLPath(""); err == nil {
		t.Fatal("empty SBPL path accepted")
	}
	if err := validateSBPLPath("bad\x00path"); err == nil {
		t.Fatal("NUL SBPL path accepted")
	}
}

func TestDarwinStartReportsMissingCommand(t *testing.T) {
	if !platformAvailable() {
		t.Skip("sandbox-exec is unavailable")
	}
	engine := &Engine{policy: normalizedPolicy{timeout: time.Second, subprocess: true, network: true}}
	if _, err := startPlatform(context.Background(), engine, []string{"missing-marxagent-command"}, nil, t.TempDir()); err == nil {
		t.Fatal("missing command did not fail")
	}
}

func TestDarwinSeatbeltBackend(t *testing.T) {
	if !platformAvailable() {
		t.Skip("sandbox-exec is unavailable")
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
