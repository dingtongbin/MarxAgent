// SPDX-License-Identifier: Apache-2.0

package skills

type Source string

const (
	SourceGlobal  Source = "global"
	SourceProject Source = "project"
)

type Config struct {
	GlobalDir    string
	ProjectDir   string
	MaxFileBytes int64
	MaxResults   int
}

type Entry struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Keywords      []string          `json:"keywords,omitempty"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	AllowedTools  string            `json:"allowed_tools,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Source        Source            `json:"source"`
	Root          string            `json:"-"`
	File          string            `json:"-"`
}

type Skill struct {
	Entry
	Instructions string `json:"instructions"`
}

type frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	Keywords      []string          `yaml:"keywords"`
	License       string            `yaml:"license"`
	Compatibility string            `yaml:"compatibility"`
	AllowedTools  string            `yaml:"allowed-tools"`
	Metadata      map[string]string `yaml:"metadata"`
}
