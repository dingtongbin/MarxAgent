// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"github.com/dingtongbin/MarxAgent/internal/engine/mcp"
	"github.com/dingtongbin/MarxAgent/internal/engine/subagent"
)

// Parts is what the assembly layer hands a mode: where its optional parts
// live. Every field is optional, and a part whose configuration is missing is
// a part the conversation runs without — a session that cannot start is a
// worse fault than a tool that is not there.
type Parts struct {
	// SkillGlobalDir holds the skills the user installed.
	SkillGlobalDir string
	// SkillProjectDir holds the skills the project ships. It shadows the
	// global directory by name.
	SkillProjectDir string
	// TemplateGlobalDir holds the sub agent templates the user installed.
	TemplateGlobalDir string
	// TemplateProjectDir holds the sub agent templates the project ships.
	TemplateProjectDir string
	// LSPCommand is the language server to run, argv with the program first.
	// Empty means the project has no server and the mode carries no lsp tools.
	LSPCommand []string
	// WikiRoot is where the project wiki lives.
	WikiRoot string
	// MCPServers are the MCP servers the conversation may call. The tools are
	// discovered lazily, so an unstarted server costs nothing.
	MCPServers []mcp.ServerConfig
	// AgentJournal is the durability sink the sub agent broker writes through.
	// Nil means the conversation carries no sub agents.
	AgentJournal subagentJournal
	// AgentMainID is the identifier the main core acts as on the sub agent
	// pool. Empty is treated the same as a missing journal.
	AgentMainID string
}

// subagentJournal is the journal the broker writes through, spelled once so
// the parts and the wiring agree about what it is.
type subagentJournal = subagent.Journal
