// SPDX-License-Identifier: Apache-2.0

package mode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/skills"
)

// The two skills tools follow the pool's own split: a cheap search for
// finding, and a full read for using. Both are read-only, so a plan turn
// carries them as it carries read and grep — planning may look at what the
// machine knows how to do.
type discoverSkillTool struct{ pool *skillsPool }
type activateSkillTool struct{ pool *skillsPool }

// skillsPool is the pool a skills tool is built on, spelled once so the two
// tools and the wiring agree about what they hold.
type skillsPool = skills.Pool

func (t *discoverSkillTool) Name() string { return "discover_skill" }

func (t *discoverSkillTool) Description() string {
	return "Search the installed skills by query and return the matches. A skill is a " +
		"set of standing instructions; call activate_skill with a name to read it in full."
}

func (t *discoverSkillTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "What the skill should be about."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`)
}

func (t *discoverSkillTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := decodeSkillArgs(raw, &args); err != nil {
		return skillRefusal(err.Error()), nil
	}
	entries, err := t.pool.Search(ctx, args.Query, 0)
	if err != nil {
		return skillRefusal(err.Error()), nil
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return skillRefusal(err.Error()), nil
	}
	return core.ToolResult{Output: encoded}, nil
}

func (t *activateSkillTool) Name() string { return "activate_skill" }

func (t *activateSkillTool) Description() string {
	return "Read one skill in full. Follow the instructions it carries for the work it " +
		"describes; they are instructions for this conversation, from the skill's file."
}

func (t *activateSkillTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The skill's name, as discover_skill reports it."
    }
  },
  "required": ["name"],
  "additionalProperties": false
}`)
}

func (t *activateSkillTool) Execute(ctx context.Context, raw json.RawMessage) (core.ToolResult, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := decodeSkillArgs(raw, &args); err != nil {
		return skillRefusal(err.Error()), nil
	}
	skill, err := t.pool.Activate(ctx, args.Name)
	if err != nil {
		return skillRefusal(err.Error()), nil
	}
	encoded, err := json.Marshal(skill)
	if err != nil {
		return skillRefusal(err.Error()), nil
	}
	return core.ToolResult{Output: encoded}, nil
}

// skillTools builds the two tools when a pool is held, and nothing otherwise.
func (c *Coding) skillTools() []core.Tool {
	if c == nil || c.skillPool == nil {
		return nil
	}
	return []core.Tool{
		&discoverSkillTool{pool: c.skillPool},
		&activateSkillTool{pool: c.skillPool},
	}
}

// skillRefusal answers a failed call with the reason, the way the gate does:
// a model that reads why it was refused can go on with its turn.
func skillRefusal(reason string) core.ToolResult {
	quoted, _ := json.Marshal(reason)
	return core.ToolResult{
		Output:  json.RawMessage(`{"error":` + string(quoted) + `}`),
		IsError: true,
		Error:   reason,
	}
}

// decodeSkillArgs reads one JSON object body.
func decodeSkillArgs(raw json.RawMessage, target any) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return fmt.Errorf("skills: the call carried no arguments")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	return nil
}
