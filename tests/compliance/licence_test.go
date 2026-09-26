// SPDX-License-Identifier: Apache-2.0

// Package compliance holds the checks that are about the repository rather than
// about its behaviour: what it depends on and under what terms.
//
// A licence is a promise made to the people who run this. The one in AGENTS is
// narrow on purpose, and a dependency that arrives under other terms does not
// have to be malicious to break it, so the terms are read from each module
// rather than remembered in a list that drifts.
package compliance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// allowed is the set of terms this project may depend on.
//
// The permissive licences are here because they place no obligation on a
// distributor beyond keeping the notice. The copyleft licences are not, and
// neither is the four clause BSD, whose advertising clause is an obligation
// rather than a courtesy.
var allowed = map[string]string{
	"Apache-2.0":   "Apache License, Version 2.0",
	"MIT":          "MIT License",
	"BSD-2-Clause": "Redistribution and use in source and binary forms",
	"BSD-3-Clause": "Neither the name of",
	"ISC":          "Permission to use, copy, modify, and/or distribute",
	"CC0-1.0":      "CC0 1.0 Universal",
	"0BSD":         "Permission to use, copy, modify, and/or distribute",
	"BSL-1.0":      "Boost Software License",
	"PostgreSQL":   "PostgreSQL License",
	"Python-2.0":   "Python License",
	"Unlicense":    "This is free and unencumbered software released into the public domain",
	"Zlib":         "This software is provided 'as-is'",
}

// module is the part of go list's output this check needs.
type module struct {
	Path     string
	Version  string
	Dir      string
	Main     bool
	Indirect bool
}

// TestEveryDependencyIsUnderATermWeMayDependOn checks the modules whose code is
// actually compiled into this repository's packages and tests.
//
// The module graph is larger than that, and a module nothing imports is not
// distributed with this project and cannot impose terms on it. Checking the
// whole graph would report modules that never reach a binary, which is how a
// guard like this starts being ignored.
func TestEveryDependencyIsUnderATermWeMayDependOn(t *testing.T) {
	built := builtModules(t)
	if len(built) == 0 {
		t.Fatal("no modules were found in the build, so nothing was checked")
	}
	available := map[string]module{}
	for _, candidate := range listModules(t) {
		available[candidate.Path] = candidate
	}
	var refused []string
	for _, path := range sortedKeys(built) {
		dependency, known := available[path]
		if !known {
			refused = append(refused, path+" (not in the module list)")
			continue
		}
		licence := identifyLicence(dependency)
		if _, ok := allowed[licence]; ok {
			continue
		}
		refused = append(refused, dependency.Path+" "+dependency.Version+" ("+licence+")")
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		t.Fatalf("%d of the %d modules in this build are not under a term it may depend on:\n  %s\n"+
			"Either the term is one of %s, or the dependency has to go.",
			len(refused), len(built), strings.Join(refused, "\n  "), strings.Join(names(allowed), ", "))
	}
}

// A module in the build with no readable licence is treated as unknown rather
// than as permissive, because silence is not permission.
func TestAModuleWithoutALicenceIsRefusedRatherThanAssumed(t *testing.T) {
	licence, ok := classify("")
	if ok {
		t.Fatalf("empty text was read as %q", licence)
	}
	// A file that exists but says nothing recognisable is equally unknown.
	if name, ok := classify("Copyright 2026 Nobody. All rights reserved."); ok {
		t.Fatalf("an unrecognisable notice was read as %q", name)
	}
}

// The direct dependencies are the ones a maintainer actually chose, so a new one
// arriving under the wrong terms should be noticed before it is merged rather
// than at the next audit.
func TestTheDirectDependenciesAreFewEnoughToKnow(t *testing.T) {
	direct := 0
	for _, candidate := range listModules(t) {
		if !candidate.Main && !candidate.Indirect {
			direct++
		}
	}
	// Every direct dependency is a decision somebody made and has to remember
	// making. A number that only ever goes up is a number nobody is choosing.
	if direct > 12 {
		t.Fatalf("there are %d direct dependencies, which is too many to have been chosen", direct)
	}
}

// The project promises to be buildable as one binary with no C toolchain, and a
// dependency that needs cgo would quietly take that away.
func TestNoDependencyNeedsCGO(t *testing.T) {
	// A package that imports "C" cannot be built without a C compiler, so the
	// guard is the same one the build job runs, applied to the dependency tree.
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go tool is not on the path")
	}
	output, err := runGo(t, "list", "-deps", "./...")
	if err != nil {
		t.Fatalf("listing the dependency tree: %v\n%s", err, output)
	}
	for _, line := range strings.Split(output, "\n") {
		// A vendored or standard package is not ours to judge here, and the
		// standard library cannot import C anyway.
		if !strings.Contains(line, ".") {
			continue
		}
		if strings.Contains(line, "runtime/cgo") {
			t.Fatalf("%q is in the dependency tree and needs a C toolchain", line)
		}
	}
}

// builtModules names the modules whose packages this repository compiles,
// including the ones only a test imports, because test code is published with
// the source and its terms travel with it.
func builtModules(t *testing.T) map[string]bool {
	t.Helper()
	output, err := runGo(t, "list", "-deps", "-test", "-f", "{{if .Module}}{{.Module.Path}}{{end}}", "./...")
	if err != nil {
		t.Fatalf("listing the build: %v\n%s", err, output)
	}
	modules := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		path := strings.TrimSpace(line)
		// The standard library has no module, and this project is not a dependency
		// of itself.
		if path == "" || !strings.Contains(path, ".") || strings.HasSuffix(path, "MarxAgent") {
			continue
		}
		modules[path] = true
	}
	return modules
}

// listModules asks the go tool where every module lives, because the cache
// encodes upper case letters and guessing the path is how a check like this
// silently passes over everything.
func listModules(t *testing.T) []module {
	t.Helper()
	output, err := runGo(t, "list", "-m", "-json", "all")
	if err != nil {
		t.Fatalf("listing the modules: %v\n%s", err, output)
	}
	var modules []module
	decoder := json.NewDecoder(strings.NewReader(output))
	for decoder.More() {
		var current module
		if err := decoder.Decode(&current); err != nil {
			t.Fatalf("reading the module list: %v", err)
		}
		modules = append(modules, current)
	}
	return modules
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func runGo(t *testing.T, arguments ...string) (string, error) {
	t.Helper()
	command := exec.Command("go", arguments...)
	// A test runs with its own package as the working directory, so a relative
	// pattern like ./... would quietly describe the test package alone and the
	// check would pass over the entire project without noticing. Every go command
	// here therefore runs from the module root.
	command.Dir = moduleRoot(t)
	// A guard that reports on the dependency set must not be able to change it, so
	// the module files are read only. With the default writable mode this test
	// quietly added entries to go.sum, which is a repository change made by a
	// command that was only meant to look at one.
	command.Env = append(os.Environ(), "GOFLAGS=-mod=readonly")
	var stderr strings.Builder
	command.Stderr = &stderr
	stdout, err := command.Output()
	if err != nil {
		return string(stdout) + stderr.String(), err
	}
	return string(stdout), nil
}

// moduleRoot is the directory holding go.mod, found through the go tool rather
// than by counting path segments, which breaks the moment a test moves.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err == nil {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root
		}
	}
	// A test that is not two directories below the root still has to work, so the
	// tool is asked where the module is.
	output, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("finding the module root: %v", err)
	}
	found := strings.TrimSpace(string(output))
	if found == "" || found == os.DevNull {
		t.Fatal("this is not inside a module, so there are no terms to check")
	}
	return filepath.Dir(found)
}

// identifyLicence names the terms a module is under, or says plainly that it
// could not be told.
func identifyLicence(dependency module) string {
	if dependency.Dir == "" {
		// A module listed without a directory is one that was never downloaded, so
		// its terms are unknown rather than absent.
		return "not downloaded, run go mod download"
	}
	entries, err := os.ReadDir(dependency.Dir)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	// A NOTICE file states what a licence cannot, and a module with one usually
	// carries the Apache terms alongside it.
	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToUpper(entry.Name())
		if strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "LICENCE") ||
			strings.HasPrefix(name, "COPYING") {
			candidates = append(candidates, filepath.Join(dependency.Dir, entry.Name()))
		}
	}
	sort.Strings(candidates)
	for _, candidate := range candidates {
		content, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		if name, ok := classify(string(content)); ok {
			return name
		}
	}
	return "no licence file"
}

// classify names the terms a licence text is under.
//
// The markers are the phrases a licence is required to contain rather than its
// filename, because a project may call its licence anything and a file called
// LICENSE may hold one of several. The order matters: the copyleft notices quote
// the Apache and BSD texts inside themselves, so a notice of copyleft has to be
// recognised before the permissive text it quotes.
func classify(text string) (string, bool) {
	normalised := strings.Join(strings.Fields(text), " ")
	// The refusals come first, because a copyleft notice quotes the permissive
	// licence it is derived from and would otherwise be read as that licence.
	for _, refusal := range []struct{ marker, name string }{
		{"GNU AFFERO GENERAL PUBLIC LICENSE", "AGPL"},
		{"GNU LESSER GENERAL PUBLIC LICENSE", "LGPL"},
		{"GNU GENERAL PUBLIC LICENSE", "GPL"},
		{"Mozilla Public License", "MPL"},
		{"The Eclipse Public License", "EPL"},
		{"This program is free software: you can redistribute it", "GPL"},
		{"All rights reserved. This program and the accompanying materials", "GPL"},
	} {
		if strings.Contains(normalised, refusal.marker) {
			return refusal.name, true
		}
	}
	// The four clause BSD carries an advertising clause, which is an obligation
	// on a distributor, so it is named apart from the three clause form.
	if strings.Contains(normalised, "All advertising materials mentioning features") {
		return "BSD-4-Clause", true
	}
	switch {
	case strings.Contains(normalised, "Apache License") && strings.Contains(normalised, "Version 2.0"):
		return "Apache-2.0", true
	case strings.Contains(normalised, "Permission is hereby granted, free of charge"):
		return "MIT", true
	case strings.Contains(normalised, "Redistribution and use in source and binary forms"):
		// Two clauses grant the redistribution, three name the copyright holder as
		// well, and a project that writes neither is a project whose terms are
		// unclear rather than permissive.
		if strings.Contains(normalised, "Neither the name of") {
			return "BSD-3-Clause", true
		}
		return "BSD-2-Clause", true
	case strings.Contains(normalised, "Permission to use, copy, modify, and/or distribute"):
		return "ISC", true
	case strings.Contains(normalised, "CC0 1.0 Universal"):
		return "CC0-1.0", true
	case strings.Contains(normalised, "Permission to use, copy, modify, and/or distribute this software"):
		return "0BSD", true
	case strings.Contains(normalised, "Boost Software License"):
		return "BSL-1.0", true
	case strings.Contains(normalised, "released into the public domain"):
		return "Unlicense", true
	case strings.Contains(normalised, "This software is provided 'as-is'"):
		return "Zlib", true
	}
	return "", false
}

func names(set map[string]string) []string {
	var listed []string
	for name := range set {
		listed = append(listed, name)
	}
	sort.Strings(listed)
	return listed
}

// A guard that cannot fail is worse than no guard, because it is read as
// evidence. These are the texts it has to be able to refuse, including the ones
// that quote a permissive licence inside a copyleft notice.
func TestTheClassifierRefusesTheTermsThisProjectMayNotDependOn(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "the GPL text",
			text: "This program is free software: you can redistribute it and/or modify " +
				"it under the terms of the GNU General Public License as published by " +
				"the Free Software Foundation; either version 2 of the License, or (at your " +
				"option) any later version.",
			want: "GPL",
		},
		{
			name: "an LGPL notice that quotes the Apache text",
			text: "GNU LESSER GENERAL PUBLIC LICENSE\nVersion 2.1\n\n" +
				"Licensed under the Apache License, Version 2.0 (the \"License\"); you may " +
				"not use this file except in compliance with the License.",
			want: "LGPL",
		},
		{
			name: "an AGPL notice",
			text: "GNU AFFERO GENERAL PUBLIC LICENSE Version 3",
			want: "AGPL",
		},
		{
			name: "the Mozilla Public License",
			text: "Mozilla Public License Version 2.0",
			want: "MPL",
		},
		{
			name: "the four clause BSD, whose advertising clause is an obligation",
			text: "Redistribution and use in source and binary forms, with or without " +
				"modification, are permitted provided that the following conditions are met: " +
				"All advertising materials mentioning features or use of this software must " +
				"display the following acknowledgement.",
			want: "BSD-4-Clause",
		},
		{
			name: "the two clause BSD",
			text: "Redistribution and use in source and binary forms, with or without " +
				"modification, are permitted provided that the following conditions are met: " +
				"THIS SOFTWARE IS PROVIDED BY THE AUTHOR ``AS IS'' AND ANY EXPRESS OR IMPLIED " +
				"WARRANTIES ARE DISCLAIMED.",
			want: "BSD-2-Clause",
		},
		{
			name: "the three clause BSD",
			text: "Redistribution and use in source and binary forms, with or without " +
				"modification, are permitted provided that the following conditions are met: " +
				"Neither the name of the copyright holder nor the names of its contributors " +
				"may be used to endorse or promote products derived from this software.",
			want: "BSD-3-Clause",
		},
		{
			name: "the MIT licence",
			text: "Permission is hereby granted, free of charge, to any person obtaining a " +
				"copy of this software and associated documentation files, to deal in the " +
				"Software without restriction.",
			want: "MIT",
		},
		{
			name: "the Apache licence",
			text: "Apache License\nVersion 2.0, January 2004\nhttp://www.apache.org/licenses/",
			want: "Apache-2.0",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, ok := classify(test.text)
			if !ok {
				t.Fatal("the text was not recognised at all")
			}
			if got != test.want {
				t.Fatalf("classified as %q, want %q", got, test.want)
			}
			// Whatever it was classified as, the project may only depend on it if it
			// is on the list, and the list is the decision, not the classifier.
			if _, permitted := allowed[got]; !permitted {
				t.Logf("%q is refused by the allowlist, which is the point", got)
			}
		})
	}
	// The permissive terms this project relies on are recognised as themselves and
	// are on the list, so a rewrite of the classifier that broke them would fail
	// here rather than in a licence audit.
	for _, name := range []string{"Apache-2.0", "MIT", "BSD-2-Clause", "BSD-3-Clause", "ISC"} {
		if _, ok := allowed[name]; !ok {
			t.Fatalf("%q is not on the allowlist this project depends on", name)
		}
	}
}

// A module the build reaches is read from the cache the go tool reports, and the
// path is not guessed: the cache encodes upper case letters, so a guessed path
// would report every such module as unlicensed and the guard would be noise.
func TestAModuleInTheCacheIsReadFromWhereTheGoToolSays(t *testing.T) {
	built := builtModules(t)
	available := map[string]module{}
	for _, candidate := range listModules(t) {
		available[candidate.Path] = candidate
	}
	var read int
	for path := range built {
		dependency, known := available[path]
		if !known || dependency.Dir == "" {
			continue
		}
		if name, ok := classify(readLicence(t, dependency.Dir)); ok {
			read++
			if _, permitted := allowed[name]; !permitted {
				t.Fatalf("%s is under %q, which is not on the allowlist", path, name)
			}
		}
	}
	if read == 0 {
		t.Fatal("not one module's licence could be read, so the guard proved nothing")
	}
	t.Logf("read the terms of %d of %d modules in the build", read, len(built))
}

func readLicence(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := strings.ToUpper(entry.Name())
		if !strings.HasPrefix(name, "LICENSE") && !strings.HasPrefix(name, "LICENCE") &&
			!strings.HasPrefix(name, "COPYING") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		return string(content)
	}
	return ""
}
