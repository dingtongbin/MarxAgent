// SPDX-License-Identifier: Apache-2.0

package core

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestL1ProductionFilesUseOnlyPureStandardLibrary(t *testing.T) {
	allowedImports := map[string]struct{}{
		"context":       {},
		"crypto/rand":   {},
		"encoding/json": {},
		"errors":        {},
		"fmt":           {},
		"math":          {},
		"reflect":       {},
		"sort":          {},
		"strconv":       {},
		"strings":       {},
		"sync":          {},
		"sync/atomic":   {},
		"time":          {},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	productionFiles := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		productionFiles++
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		if !strings.HasPrefix(string(contents), "// SPDX-License-Identifier: Apache-2.0\n") {
			t.Errorf("%s is missing the Apache-2.0 SPDX header", path)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, contents, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%q) error = %v", path, err)
		}
		for _, importSpec := range parsed.Imports {
			importPath, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				t.Errorf("%s has an invalid import path: %v", path, err)
				continue
			}
			if _, allowed := allowedImports[importPath]; !allowed {
				t.Errorf("%s imports forbidden L1 package %q", path, importPath)
			}
		}
	}
	if productionFiles == 0 {
		t.Fatal("no production Go files found")
	}
}
