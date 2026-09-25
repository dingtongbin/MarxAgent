// SPDX-License-Identifier: Apache-2.0
//go:build windows

package tools

import (
	"strings"
	"testing"
)

func TestWindowsPathWithinRejectsCrossVolumePaths(t *testing.T) {
	root := t.TempDir()
	if !pathWithin(root, root) {
		t.Fatal("root does not contain itself")
	}
	if !pathWithin(root, strings.ToUpper(root)) {
		t.Fatal("Windows path comparison is case sensitive")
	}
	if pathWithin(root, `Z:\elsewhere`) {
		t.Fatal("cross-volume path was treated as contained")
	}
}

func TestRelativeFallsBackWhenRelFails(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), WorkspaceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := workspace.relative(`Z:\outside\file.txt`); got != "Z:/outside/file.txt" {
		t.Fatalf("relative = %q", got)
	}
}
