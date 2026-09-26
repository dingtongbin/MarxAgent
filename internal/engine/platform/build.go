// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"fmt"
	"sort"
	"strings"
)

// Target is one operating system and processor the project builds for.
type Target struct {
	// GOOS is the Go name for the operating system.
	GOOS string `json:"goos"`
	// GOARCH is the Go name for the processor.
	GOARCH string `json:"goarch"`
}

// String is the pair as a build command writes it.
func (t Target) String() string { return t.GOOS + "/" + t.GOARCH }

// Env is the environment a cross build for this target needs.
//
// CGO is off for every one of them, and that is not a preference. The whole project
// is meant to be one binary per platform with nothing to install alongside it, and a
// single cgo dependency would make that false: the binary would need a C toolchain
// at build time and a matching libc on the machine it ran on. Keeping it off here
// means a target cannot be added that quietly needs it, because the build would fail
// rather than produce something that only works on the machine that built it.
func (t Target) Env() map[string]string {
	return map[string]string{
		"GOOS":        t.GOOS,
		"GOARCH":      t.GOARCH,
		"CGO_ENABLED": "0",
		"GOFLAGS":     "-trimpath",
		"GO111MODULE": "on",
		"GOTOOLCHAIN": "local",
	}
}

// Targets are the combinations the project promises to build for.
//
// All three systems are first class and both processors are built everywhere, so the
// list is a rectangle rather than a set of convenient cases. A list written as the
// cases that happened to be convenient is how a platform quietly stops being
// supported without anyone deciding it should.
var Targets = []Target{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
	{GOOS: "windows", GOARCH: "amd64"},
	{GOOS: "windows", GOARCH: "arm64"},
}

// BuildCommand is the whole command that builds one target, environment included.
//
// The target goes in the environment rather than in the arguments, because that is
// how the Go toolchain selects a platform, and a caller that has to remember which
// half carries the target is a caller that will eventually get it wrong. Spelling
// both out together means the returned value can be run as it stands, and a person
// can copy it to reproduce a build by hand.
func BuildCommand(target Target) []string {
	command := []string{"go", "build", "./..."}
	for _, name := range sortedKeys(target.Env()) {
		command = append([]string{name + "=" + target.Env()[name]}, command...)
	}
	return command
}

// sortedKeys keeps the command the same every time it is built.
//
// The environment is a map, and a map's order is not fixed, so a command assembled by
// walking one would come out in a different order each time. That is harmless to run
// and very confusing to read in a log.
func sortedKeys(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// VerifyTargets reports which of the promised combinations are missing from a list.
//
// It takes a list rather than checking for itself on purpose: the check belongs to
// whatever is assembling the build, and this only knows what the answer should look
// like. A check that ran itself would pass on a machine where nothing was being
// built and report success about a claim it never tested.
func VerifyTargets(present []Target) error {
	have := make(map[Target]bool, len(present))
	for _, target := range present {
		have[target] = true
	}
	var missing []string
	for _, target := range Targets {
		if !have[target] {
			missing = append(missing, target.String())
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"platform: %d of %d promised build targets are missing: %s",
		len(missing), len(Targets), strings.Join(missing, ", "))
}
