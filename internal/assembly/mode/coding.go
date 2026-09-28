// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/lsp"
	"github.com/dingtongbin/MarxAgent/internal/engine/mcp"
	"github.com/dingtongbin/MarxAgent/internal/engine/sandbox"
	"github.com/dingtongbin/MarxAgent/internal/engine/security"
	"github.com/dingtongbin/MarxAgent/internal/engine/skills"
	"github.com/dingtongbin/MarxAgent/internal/engine/subagent"
	"github.com/dingtongbin/MarxAgent/internal/engine/tools"
	"github.com/dingtongbin/MarxAgent/internal/engine/wiki"
)

// CodingMaxIterations is the reasoning and tool loop budget of a coding turn.
//
// A coding turn is a search for the change and then the change itself, and the
// search is a chain of read, grep and shell calls that a conversational turn has
// no use for. The budget is still finite, because a loop that never converged is
// a fault the model should be told about rather than an afternoon.
const CodingMaxIterations = 40

// Coding assembles the coding mode for one project directory.
//
// The assembly is built once per conversation and registered on each turn's
// fresh agent, because the agent is thrown away per turn while the tools, the
// gates and the standing context are properties of the project the conversation
// is about. An assembly that was rebuilt per turn would re-walk the project and
// re-open the shell every message, for nothing.
//
// The execution mode the turn is sent under decides which reach the turn is
// assembled at. The reach is a property of the project and of nothing else,
// so each level's tools and gate are built once here and handed out from
// then on: a conversation that moves between plan, build and full access
// re-uses the same snapshot, the same shell and the same tools rather than
// opening any of them again.
type Coding struct {
	root      string
	workspace *tools.Workspace
	snapshot  string

	// The optional parts. Each is nil when its configuration is missing, and a
	// nil part is a part the conversation runs without rather than a fault.
	skillPool     *skills.Pool
	lspPool       *lsp.Pool
	lspInjector   *lsp.DiagnosticsInjector
	wikiStore     *wiki.Store
	wikiRetriever *wiki.Retriever
	mcpPool       *mcp.Pool
	orchestrator  *subagent.Orchestrator

	kitMu sync.Mutex
	kits  map[ExecutionMode]*execKit
}

// execKit is the reach of one execution level: the tools the turn is given
// and the gate they are behind. A level with no gate carries a nil policy,
// which is the full-access level's way of refusing nothing.
type execKit struct {
	tools  []core.Tool
	policy security.Policy
}

// NewCoding builds the assembly for a project directory.
//
// The sandboxes are best effort on purpose: a platform whose backend is
// unavailable still gets a shell, because the build being unable to run a
// command at all would be a coding mode that cannot code. The direct runner is
// the fallback, and the tool gates are the reach that still bounds the commands.
// NewCoding builds the assembly for a project directory.
//
// The sandbox is best effort on purpose: a platform whose backend is unavailable
// still gets a shell, because the build being unable to run a command at all
// would be a coding mode that cannot code. The direct runner is the fallback,
// and the tool gates are the reach that still bounds the commands.
//
// The optional parts are each taken when they can be and skipped when they
// cannot: a skills directory that cannot be opened, a language server that
// cannot be built or a wiki that cannot be created are parts the conversation
// runs without, because a session that fails to start is a worse fault than a
// tool that is not there.
func NewCoding(root string, parts Parts) (*Coding, error) {
	cleaned := filepath.Clean(root)
	if cleaned == "" || cleaned == "." {
		return nil, fmt.Errorf("mode: the coding mode needs a project directory")
	}
	workspace, err := tools.NewWorkspace(cleaned, tools.WorkspaceOptions{})
	if err != nil {
		return nil, fmt.Errorf("mode: open the project workspace: %w", err)
	}
	assembly := &Coding{
		root:      workspace.Root(),
		workspace: workspace,
		snapshot:  Snapshot(workspace.Root()),
		kits:      make(map[ExecutionMode]*execKit),
	}
	assembly.openSkills(parts)
	assembly.openLSP(parts)
	assembly.openWiki(parts)
	assembly.openMCP(parts)
	assembly.openAgents(parts)
	return assembly, nil
}

// Coding tool budgets. The diagnostics injector caps how many problems stand
// in front of a turn, and the retriever caps how many wiki hits do.
const (
	lspDefaultProblems = 12
	wikiDefaultTopK    = 4
)

// openSkills builds the skills pool over its two directories and reads the
// index once. A pool that cannot be built is left out entirely.
func (c *Coding) openSkills(parts Parts) {
	if parts.SkillGlobalDir == "" && parts.SkillProjectDir == "" {
		return
	}
	pool, err := skills.NewPool(skills.Config{
		GlobalDir:  parts.SkillGlobalDir,
		ProjectDir: parts.SkillProjectDir,
	})
	if err != nil {
		return
	}
	_ = pool.Refresh(context.Background())
	c.skillPool = pool
}

// openLSP builds the language server's pool and its diagnostics injector. The
// server itself starts lazily, so a conversation that never asks it a question
// never starts it.
func (c *Coding) openLSP(parts Parts) {
	if len(parts.LSPCommand) == 0 {
		return
	}
	client, err := lsp.New(lsp.Config{
		Command:   parts.LSPCommand,
		Workspace: c.root,
	})
	if err != nil {
		return
	}
	pool, err := lsp.NewPool(client)
	if err != nil {
		return
	}
	injector, err := lsp.NewDiagnosticsInjector(pool, lspDefaultProblems)
	if err != nil {
		_ = pool.Close()
		return
	}
	c.lspPool = pool
	c.lspInjector = injector
}

// openWiki opens the project wiki and its retriever. A directory that cannot
// be created is a wiki the conversation runs without.
func (c *Coding) openWiki(parts Parts) {
	if parts.WikiRoot == "" {
		return
	}
	store, err := wiki.NewStore(parts.WikiRoot)
	if err != nil {
		return
	}
	retriever, err := wiki.NewRetriever(store, wikiDefaultTopK)
	if err != nil {
		return
	}
	c.wikiStore = store
	c.wikiRetriever = retriever
}

// openMCP builds the pool of servers. The tools are discovered lazily on the
// turn that asks for them.
func (c *Coding) openMCP(parts Parts) {
	if len(parts.MCPServers) == 0 {
		return
	}
	pool, err := mcp.NewPool(parts.MCPServers)
	if err != nil {
		return
	}
	c.mcpPool = pool
}

// openAgents assembles the sub agent infrastructure: the broker over the
// journal, the registry that owns the population, the templates and the
// builder that confines a sub agent to the tools it names. A part that fails
// to build is a part the conversation runs without.
func (c *Coding) openAgents(parts Parts) {
	if parts.AgentJournal == nil || strings.TrimSpace(parts.AgentMainID) == "" {
		return
	}
	templates, err := subagent.NewTemplatePool(subagent.TemplateConfig{
		GlobalDir:  parts.TemplateGlobalDir,
		ProjectDir: parts.TemplateProjectDir,
	})
	if err != nil {
		return
	}
	if err := templates.Refresh(context.Background()); err != nil {
		return
	}
	mainID := strings.TrimSpace(parts.AgentMainID)
	broker, err := subagent.NewBroker(subagent.BrokerConfig{
		Journal: parts.AgentJournal,
		MainID:  mainID,
	})
	if err != nil {
		return
	}
	registry, err := subagent.NewRegistry(subagent.RegistryConfig{
		MainID: mainID,
		Broker: broker,
	})
	if err != nil {
		return
	}
	pool, err := subagent.NewPool(subagent.PooledAgentConfig{
		MainID:    mainID,
		Registry:  registry,
		Broker:    broker,
		Templates: templates,
	})
	if err != nil {
		return
	}
	orchestrator, err := subagent.NewOrchestrator(subagent.OrchestrationConfig{
		Pool:   pool,
		MainID: mainID,
	})
	if err != nil {
		return
	}
	c.orchestrator = orchestrator
}

// Root is the project directory the assembly is confined to.
func (c *Coding) Root() string {
	if c == nil {
		return ""
	}
	return c.root
}

// Tools returns the tool set of one reach level: that level's built-in parts,
// then the skills, the language server, the wiki, the MCP servers and the sub
// agent orchestration, each group in its own stable order. A part that is not
// held contributes nothing.
//
// The context travels with the call because an MCP server's tools are
// discovered on the turn that asks for them: an unstarted server starts here,
// lazily, the first time the conversation reaches for it.
func (c *Coding) Tools(ctx context.Context, exec ExecutionMode) ([]core.Tool, error) {
	if c == nil || c.workspace == nil {
		return nil, fmt.Errorf("mode: the coding assembly is not built")
	}
	if !exec.Valid() {
		return nil, fmt.Errorf("mode: unknown execution mode %q", exec)
	}
	c.kitMu.Lock()
	kit, exists := c.kits[exec]
	c.kitMu.Unlock()
	if !exists {
		var err error
		kit, err = c.buildKit(exec)
		if err != nil {
			return nil, err
		}
		c.kitMu.Lock()
		c.kits[exec] = kit
		c.kitMu.Unlock()
	}
	built := append(append([]core.Tool(nil), kit.tools...), c.skillTools()...)
	built = append(built, c.lspTools()...)
	built = append(built, c.wikiTools()...)
	built = append(built, c.mcpTools(ctx)...)
	built = append(built, c.agentTools()...)
	return built, nil
}

// lspTools is the language server's five tools, in the pool's own order.
func (c *Coding) lspTools() []core.Tool {
	if c == nil || c.lspPool == nil {
		return nil
	}
	return c.lspPool.Tools()
}

// wikiTools is the wiki's two tools.
func (c *Coding) wikiTools() []core.Tool {
	if c == nil || c.wikiStore == nil {
		return nil
	}
	return c.wikiStore.Tools()
}

// mcpTools is what the held servers expose, discovered on this call: a server
// that has not started starts here, lazily, and one that fails to answer
// contributes nothing rather than taking the turn down.
func (c *Coding) mcpTools(ctx context.Context) []core.Tool {
	if c == nil || c.mcpPool == nil {
		return nil
	}
	tools, err := c.mcpPool.Tools(ctx)
	if err != nil {
		return nil
	}
	return tools
}

// agentTools is the sub agent orchestration: the eight tools the main core
// uses to create, direct and read its sub agents.
func (c *Coding) agentTools() []core.Tool {
	if c == nil || c.orchestrator == nil {
		return nil
	}
	return c.orchestrator.Tools()
}

// buildKit assembles one level's reach. The parts differ by level: plan reads
// and searches, build edits and runs inside the workspace, and full runs
// everything with the gates off.
func (c *Coding) buildKit(exec ExecutionMode) (*execKit, error) {
	switch exec {
	case ExecPlan:
		kit, err := c.planKit()
		if err != nil {
			return nil, err
		}
		return kit, nil
	case ExecFull:
		kit, err := c.fullKit()
		if err != nil {
			return nil, err
		}
		return kit, nil
	default:
		kit, err := c.buildLevelKit()
		if err != nil {
			return nil, err
		}
		return kit, nil
	}
}

// planKit is the read-and-search reach: the read tool, the search tools and an
// allowlist that names nothing else. There is no shell in it, because a shell
// is how a plan stops being one.
func (c *Coding) planKit() (*execKit, error) {
	readOnly := &security.AllowlistPolicy{
		Tools: []string{"read", "glob", "grep"},
	}
	return &execKit{
		tools:  readOnlyFileTools(c.workspace),
		policy: readOnly,
	}, nil
}

// fullKit is the ungated reach: the same six tools as build, with the shell
// running without a sandbox and no policy at all. Nothing here refuses
// anything; the level is the user saying the model may act without asking.
func (c *Coding) fullKit() (*execKit, error) {
	shell, err := c.shellTool(sandbox.NewUnrestricted())
	if err != nil {
		return nil, err
	}
	fileTools := c.workspace.BuiltinTools()
	return &execKit{
		tools: append(append([]core.Tool(nil), fileTools...), shell),
	}, nil
}

// buildLevelKit is the ordinary reach: the six tools, the shell inside its
// sandbox, and the workspace policy behind them.
func (c *Coding) buildLevelKit() (*execKit, error) {
	runner, err := sandbox.New(sandbox.Policy{
		Workspace:         c.root,
		WorkspaceWritable: true,
		Subprocess:        true,
		Network:           false,
	})
	if err != nil {
		runner = sandbox.NewUnrestricted()
	}
	shell, err := c.shellTool(runner)
	if err != nil {
		return nil, err
	}
	fileTools := c.workspace.BuiltinTools()
	return &execKit{
		tools:  append(append([]core.Tool(nil), fileTools...), shell),
		policy: codingPolicy(c.root),
	}, nil
}

// readOnlyFileTools picks the read-only tools out of the workspace's builtins,
// keeping the pool's own order.
func readOnlyFileTools(workspace *tools.Workspace) []core.Tool {
	kept := []string{"read", "glob", "grep"}
	var out []core.Tool
	for _, tool := range workspace.BuiltinTools() {
		for _, name := range kept {
			if tool.Name() == name {
				out = append(out, tool)
				break
			}
		}
	}
	return out
}

// shellTool builds a shell tool on the runner it is given, once per runner.
// The sandbox and the unrestricted runner are different shells — one bounds
// the commands and one does not — so they are not the same tool and they are
// cached separately by the kits that hold them.
func (c *Coding) shellTool(runner sandbox.Runner) (core.Tool, error) {
	shell, err := tools.NewShellTool(tools.ShellConfig{
		WorkingDirectory: c.root,
		Sandbox:          runner,
	})
	if err != nil {
		return nil, fmt.Errorf("mode: build the shell tool: %w", err)
	}
	return shell, nil
}

// SystemPrompt is the standing instruction of a coding session, followed by the
// project snapshot. The snapshot is the tail of the system text, which is where
// a prompt version bump and a project the conversation moved to both stay
// outside the part of the prefix that repeats between conversations.
func (c *Coding) SystemPrompt() string {
	prompt := CodingPrompt
	if c != nil && c.snapshot != "" {
		prompt += "\n\n# Project context\n\n" + c.snapshot
	}
	return prompt
}

// RegisterHooks puts the reach of one execution level on an agent: the tool
// gate where the level has one, and the untrusted envelope for results.
//
// Full access registers no gate, because the level is the absence of one; the
// envelope stays, since it is a fact about where results came from rather than
// a limit on what the model may do.
func (c *Coding) RegisterHooks(agent core.Agent, exec ExecutionMode) error {
	if c == nil || agent == nil {
		return fmt.Errorf("mode: register hooks on a built agent")
	}
	if !exec.Valid() {
		return fmt.Errorf("mode: unknown execution mode %q", exec)
	}
	c.kitMu.Lock()
	kit, exists := c.kits[exec]
	c.kitMu.Unlock()
	if !exists {
		// The level has not been built yet; building it is the caller's next
		// step, and a gate asked for before its kit exists is a call made out
		// of order.
		return fmt.Errorf("mode: the %q execution kit is not built", exec)
	}
	if kit.policy != nil {
		if err := agent.Hooks().Register(core.HookToolCallReceived, -50, security.Gate(kit.policy)); err != nil {
			return fmt.Errorf("mode: register the tool gate: %w", err)
		}
	}
	if err := agent.Hooks().Register(core.HookToolResult, 50, security.Wrap()); err != nil {
		return fmt.Errorf("mode: register the result envelope: %w", err)
	}
	if err := agent.Hooks().Register(core.HookContextInject, -30, c.injectContext); err != nil {
		return fmt.Errorf("mode: register the context injection: %w", err)
	}
	// The diagnostics injector and the wiki retriever each bring their own
	// hook: both are standing-context injectors that stay silent when they have
	// nothing to say, so registering one that is not held is not a fault here.
	if c.lspInjector != nil {
		if err := agent.Hooks().Register(core.HookContextInject, -20, c.lspInjector.Hook()); err != nil {
			return fmt.Errorf("mode: register the diagnostics injection: %w", err)
		}
	}
	if c.wikiRetriever != nil {
		if err := agent.Hooks().Register(core.HookContextInject, -10, c.wikiRetriever.Hook()); err != nil {
			return fmt.Errorf("mode: register the wiki injection: %w", err)
		}
	}
	return nil
}

// injectContext appends the project snapshot to the request as a system
// message, which the formats hoist into the system prompt. It is the standing
// context rather than this turn's news, so it is the same bytes every turn:
// a stable tail is a cacheable tail.
func (c *Coding) injectContext(_ context.Context, data any) (any, error) {
	request, ok := data.(core.ChatRequest)
	if !ok {
		return data, fmt.Errorf("mode: context injection received %T, want core.ChatRequest", data)
	}
	if c == nil || c.snapshot == "" {
		return request, nil
	}
	request.Messages = append(request.Messages, core.Message{
		ID:     "project-context",
		Role:   core.RoleSystem,
		Content: []core.ContentBlock{{Type: core.ContentTypeText, Text: c.snapshot}},
	})
	return request, nil
}

// Close releases the parts the assembly holds: the wiki index, the language
// server and the sub agent pool. Each part closes in its own way, and one
// that fails is reported with the rest rather than taking the rest down.
func (c *Coding) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	var firstErr error
	record := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	if c.wikiStore != nil {
		if err := c.wikiStore.Close(); err != nil {
			record(err)
		}
	}
	if c.lspPool != nil {
		if err := c.lspPool.Close(); err != nil {
			record(err)
		}
	}
	if c.orchestrator != nil {
		if err := c.orchestrator.Close(); err != nil {
			record(err)
		}
	}
	c.skillPool = nil
	c.lspPool = nil
	c.lspInjector = nil
	c.wikiStore = nil
	c.wikiRetriever = nil
	c.mcpPool = nil
	c.orchestrator = nil
	return firstErr
}

// codingPolicy composes the reach a coding session has: only the assembled
// tools, and only inside the project.
//
// The command list is a whitelist rather than a blacklist because a blacklist
// is a list of the escapes somebody has already thought of. The commands listed
// are the ones that build, test, inspect and version-control inside the
// project; everything else, including any shell inside a shell, is refused by
// not being on the list, and the refusal says so. The optional tools ride the
// same allowlist: a skill read, an lsp read and a wiki read are evidence the
// same way a file read is.
func codingPolicy(root string) security.Policy {
	permitted := &security.AllowlistPolicy{
		Tools: []string{
			"read", "write", "edit", "glob", "grep", "bash",
			"discover_skill", "activate_skill",
			"lsp_definition", "lsp_references", "lsp_diagnostics", "lsp_hover", "lsp_rename",
			"wiki_search", "wiki_read",
		},
	}
	workspace := &security.WorkspacePolicy{
		Roots: []string{root},
		AllowCommands: []string{
			"go", "gofmt", "govulncheck",
			"git",
			"make", "cmake", "ninja",
			"npm", "npx", "pnpm", "yarn", "node", "deno", "bun", "tsc", "vite", "eslint", "prettier",
			"python", "python3", "pip", "pip3", "pytest", "ruff", "mypy",
			"cargo", "rustc", "rustup",
			"java", "javac", "mvn", "gradle",
			"ruby", "gem", "bundle", "rake",
			"ls", "cat", "head", "tail", "wc", "diff", "stat", "file", "find",
			"mkdir", "touch", "cp", "mv",
			"tar", "unzip", "gzip",
			"sed", "awk", "sort", "cut", "tr", "tee",
			"pwd", "which", "env", "echo", "true", "false",
			"dir", "type", "where", "copy", "move", "more",
		},
		DangerousCommands: []string{
			"rm", "rmdir", "del", "erase", "rd",
			"sudo", "doas", "su",
			"shutdown", "reboot", "halt", "poweroff",
			"dd", "mkfs", "fdisk", "diskpart",
			"format",
			"chmod", "chown", "attrib", "icacls",
			"kill", "killall", "taskkill", "pkill",
			"curl", "wget", "Invoke-WebRequest", "iwr",
			"ssh", "scp", "sftp",
			"reg", "regedit", "setx",
			"powershell", "pwsh", "cmd",
			"docker", "podman",
			"apt", "apt-get", "yum", "dnf", "brew", "choco", "winget", "scoop",
			"systemctl", "service",
			"crontab",
		},
	}
	return security.NewChain(permitted, workspace)
}
