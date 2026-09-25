// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestL2ProductionFilesRespectLayerAndPlatformRules(t *testing.T) {
	productionFiles := 0
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		productionFiles++
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(contents)
		if !strings.HasPrefix(text, "// SPDX-License-Identifier: Apache-2.0\n") {
			t.Errorf("%s is missing the Apache-2.0 SPDX header", path)
		}
		if strings.Contains(text, "runtime.GOOS") {
			t.Errorf("%s uses runtime.GOOS instead of platform files", path)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, contents, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, importSpec := range parsed.Imports {
			importPath, err := strconv.Unquote(importSpec.Path.Value)
			if err != nil {
				t.Errorf("%s has invalid import path: %v", path, err)
				continue
			}
			if importPath == "C" || strings.Contains(importPath, "internal/assembly") {
				t.Errorf("%s imports forbidden package %q", path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if productionFiles == 0 {
		t.Fatal("no L2 production files found")
	}
}
