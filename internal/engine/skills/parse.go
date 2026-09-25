// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

func splitFrontmatter(data []byte) ([]byte, []byte, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	firstEnd := lineEnd(data, 0)
	if firstEnd < 0 || !bytes.Equal(bytes.TrimSpace(data[:firstEnd]), []byte("---")) {
		return nil, nil, fmt.Errorf("frontmatter must start with ---")
	}
	start := firstEnd
	if start < len(data) && data[start] == '\r' {
		start++
	}
	if start < len(data) && data[start] == '\n' {
		start++
	}
	for position := start; position < len(data); {
		end := lineEnd(data, position)
		if end < 0 {
			return nil, nil, fmt.Errorf("frontmatter is not closed")
		}
		line := bytes.TrimSpace(data[position:end])
		if bytes.Equal(line, []byte("---")) {
			bodyStart := end
			if bodyStart < len(data) && data[bodyStart] == '\r' {
				bodyStart++
			}
			if bodyStart < len(data) && data[bodyStart] == '\n' {
				bodyStart++
			}
			return data[start:position], data[bodyStart:], nil
		}
		position = end
		if position < len(data) && data[position] == '\n' {
			position++
		}
	}
	return nil, nil, fmt.Errorf("frontmatter is not closed")
}

func lineEnd(data []byte, start int) int {
	if start < 0 || start >= len(data) {
		return -1
	}
	for index := start; index < len(data); index++ {
		if data[index] == '\n' {
			return index
		}
	}
	return len(data)
}

func yamlUnmarshal(data []byte, out *frontmatter) error {
	if out == nil {
		return fmt.Errorf("frontmatter output must not be nil")
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse frontmatter: %w", err)
	}
	return nil
}

func (p *Pool) parseEntry(root, file string, data []byte, source Source) (Entry, error) {
	var front frontmatter
	if err := parseFrontmatter(data, &front); err != nil {
		return Entry{}, fmt.Errorf("%w: %s: %v", ErrInvalidSkill, file, err)
	}
	directoryName := filepath.Base(root)
	if front.Name != directoryName {
		return Entry{}, fmt.Errorf("%w: name %q does not match directory %q", ErrInvalidSkill, front.Name, directoryName)
	}
	if source != SourceGlobal && source != SourceProject {
		return Entry{}, fmt.Errorf("%w: source %q", ErrInvalidSkill, source)
	}
	keywords := normalizeKeywords(front.Keywords)
	if len(keywords) == 0 {
		keywords = normalizeKeywords(tokenize(front.Description))
	}
	return Entry{
		Name:          front.Name,
		Description:   strings.TrimSpace(front.Description),
		Keywords:      keywords,
		License:       strings.TrimSpace(front.License),
		Compatibility: strings.TrimSpace(front.Compatibility),
		AllowedTools:  strings.TrimSpace(front.AllowedTools),
		Metadata:      cloneMetadata(front.Metadata),
		Source:        source,
		Root:          filepath.Clean(root),
		File:          filepath.Clean(file),
	}, nil
}

func resourcePath(root, relative string) (string, error) {
	if strings.TrimSpace(relative) == "" || strings.IndexByte(relative, 0) >= 0 {
		return "", ErrResourceOutside
	}
	if filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", ErrResourceOutside
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrResourceOutside
	}
	candidate := filepath.Join(root, clean)
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return candidate, nil
		}
		return "", fmt.Errorf("skills: resolve resource %s: %w", relative, err)
	}
	if !pathWithin(root, resolved) {
		return "", ErrResourceOutside
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("skills: resource is not a regular file: %s", relative)
	}
	return resolved, nil
}
