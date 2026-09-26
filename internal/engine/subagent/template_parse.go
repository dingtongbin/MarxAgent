// SPDX-License-Identifier: Apache-2.0

package subagent

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// splitTemplateFrontmatter separates a template's header from its body.
//
// It is written out rather than taken from a library because a template is a file a
// person edits by hand, and the failure a person has to diagnose is the one they can
// see: a header that is not opened, or not closed, is two lines of their file.
func splitTemplateFrontmatter(data []byte) ([]byte, []byte, error) {
	// A byte order mark is invisible in an editor and fatal to a parser, and a file
	// saved by an editor on Windows is the most likely place for one to appear.
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	firstEnd := templateLineEnd(data, 0)
	if firstEnd < 0 || !bytes.Equal(bytes.TrimSpace(data[:firstEnd]), []byte("---")) {
		return nil, nil, fmt.Errorf("the template must start with a --- line")
	}
	start := firstEnd
	if start < len(data) && data[start] == '\r' {
		start++
	}
	if start < len(data) && data[start] == '\n' {
		start++
	}
	for position := start; position < len(data); {
		end := templateLineEnd(data, position)
		if end < 0 {
			return nil, nil, fmt.Errorf("the template header is not closed")
		}
		if bytes.Equal(bytes.TrimSpace(data[position:end]), []byte("---")) {
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
	return nil, nil, fmt.Errorf("the template header is not closed")
}

func templateLineEnd(data []byte, start int) int {
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

// parseTemplate reads the header and returns the body as the instructions.
//
// The body is returned with its surrounding blank lines removed and nothing else
// touched, because it is a prompt and a prompt somebody edited should reach the
// model as they wrote it.
func parseTemplate(data []byte) (templateFrontmatter, string, error) {
	var front templateFrontmatter
	header, body, err := splitTemplateFrontmatter(data)
	if err != nil {
		return front, "", err
	}
	if err := yaml.Unmarshal(header, &front); err != nil {
		return front, "", fmt.Errorf("parse the template header: %w", err)
	}
	instructions := strings.Trim(string(body), "\n")
	if strings.TrimSpace(instructions) == "" {
		return front, "", fmt.Errorf("the template has no instructions below its header")
	}
	if !utf8.ValidString(instructions) {
		return front, "", fmt.Errorf("the instructions are not valid UTF-8")
	}
	return front, instructions, nil
}

// validateTemplate rejects a template that cannot be used as written.
//
// Every rule here exists because the alternative is a sub agent that runs with less
// than somebody meant, and nobody finds out until it has already used it.
func validateTemplate(front templateFrontmatter, directoryName string) error {
	if strings.TrimSpace(directoryName) == "" {
		return fmt.Errorf("a template needs a directory name")
	}
	if err := validateTemplateName(directoryName); err != nil {
		return err
	}
	if strings.TrimSpace(front.Description) == "" {
		return fmt.Errorf("a template needs a description, which is what the main core reads to choose it")
	}
	if utf8.RuneCountInString(front.Description) > maxTemplateDescriptionRunes {
		return fmt.Errorf("the description is %d characters, over the %d limit",
			utf8.RuneCountInString(front.Description), maxTemplateDescriptionRunes)
	}
	if len(front.Tools) > maxTemplateTools {
		return fmt.Errorf("the template lists %d tools, over the %d limit", len(front.Tools), maxTemplateTools)
	}
	for _, tool := range front.Tools {
		if strings.TrimSpace(tool) == "" {
			return fmt.Errorf("the tool list has a blank entry")
		}
		if err := validateTemplateName(tool); err != nil {
			return fmt.Errorf("tool %q: %w", tool, err)
		}
	}
	if front.MaxIterations < 0 {
		return fmt.Errorf("max_iterations is %d, which is not a count", front.MaxIterations)
	}
	if front.MaxIterations > maxTemplateIterations {
		return fmt.Errorf("max_iterations is %d, over the %d limit", front.MaxIterations, maxTemplateIterations)
	}
	return nil
}

const (
	// maxTemplateNameRunes keeps a name short enough to type in a tool call and to
	// read in an audit without it becoming a line of its own.
	maxTemplateNameRunes = 48
	// maxTemplateDescriptionRunes is one or two sentences. The description is what
	// the main core reads to decide, and a paragraph there is a decision made by
	// reading rather than by choosing.
	maxTemplateDescriptionRunes = 240
	// maxTemplateIterations bounds what a sub agent may think about on its own. The
	// main core is waiting, and a sub agent that keeps thinking spends a budget
	// without returning anything.
	maxTemplateIterations = 64
)

// validateTemplateName accepts a lowercase, dash separated name.
//
// The rules exist so a name is usable as a directory, as a tool argument and as a
// journal subject on all three platforms. Upper case is refused because Windows and
// macOS treat it differently, and a name that behaves differently depending on the
// machine is a name nobody can rely on.
func validateTemplateName(name string) error {
	if name == "" {
		return fmt.Errorf("a name must not be empty")
	}
	if utf8.RuneCountInString(name) > maxTemplateNameRunes {
		return fmt.Errorf("the name is longer than %d characters", maxTemplateNameRunes)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("the name %q starts or ends with a dash", name)
	}
	if strings.Contains(name, "--") {
		return fmt.Errorf("the name %q has two dashes in a row", name)
	}
	for _, character := range name {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= '0' && character <= '9':
		case character == '-':
		case character == '_':
		case character == '.':
		case unicode.IsLetter(character) && character < unicode.MaxASCII:
			// A letter that is not ASCII and not in a-z, which is the accented and
			// non-Latin case a name could otherwise be written in.
			return fmt.Errorf("the name %q contains %q, which is not portable", name, character)
		default:
			return fmt.Errorf("the name %q contains %q", name, character)
		}
	}
	return nil
}
