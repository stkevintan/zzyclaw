package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"zzy/agent/skill"
	"zzy/agent/tools"
)

// runSkillTool executes a skill's code inside the Deno sandbox. Only skills that
// declare `runtime: deno` are runnable. By default a skill runs read-only with
// access to its own directory and the workspace and no network. A skill may opt
// into workspace writes (`write: true`) or network access (`net: <hosts>`); when
// it does, running it is treated as dangerous and goes through the approval and
// owner gate. Read-only, no-network skills run without a prompt.
type runSkillTool struct {
	reg       *skill.Registry
	runner    *tools.DenoRunner
	workspace string
}

// RunSkillTool builds the run_skill tool. runner executes the Deno sandbox;
// workspace is the directory granted to skills (read-only by default).
func RunSkillTool(reg *skill.Registry, runner *tools.DenoRunner, workspace string) tools.Tool {
	return &runSkillTool{reg: reg, runner: runner, workspace: workspace}
}

func (t *runSkillTool) Name() string { return "run_skill" }
func (t *runSkillTool) Description() string {
	return "Run a sandboxed skill's code (skills with `runtime: deno`). The code runs in the Deno sandbox: by default it may only read its own directory and the workspace, with no network. Skills that declare `write: true` or `net: <hosts>` get those capabilities and require approval. The skill's stdout is returned. Use list_skills to discover skills."
}
func (t *runSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","description":"Name of the skill to run."},"args":{"type":"array","items":{"type":"string"},"description":"Arguments passed to the skill program."}},"required":["skill"]}`)
}

// Dangerous returns true when the targeted skill requests elevated capabilities
// (workspace write or network), so the engine prompts for approval first.
// Read-only, no-network skills run without a prompt.
func (t *runSkillTool) Dangerous(args json.RawMessage) bool {
	var a struct {
		Skill string `json:"skill"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.Skill == "" {
		return false
	}
	s, ok := t.reg.Get(a.Skill)
	if !ok || s.Runtime != "deno" {
		return false
	}
	return s.Write || len(s.Net) > 0
}

func (t *runSkillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Skill string   `json:"skill"`
		Args  []string `json:"args"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Skill == "" {
		return "", fmt.Errorf("skill name must not be empty")
	}
	_ = t.reg.Reload()
	s, ok := t.reg.Get(a.Skill)
	if !ok {
		return "", fmt.Errorf("unknown skill %q", a.Skill)
	}
	// User-added skills must run in the Deno sandbox; only builtin skills are
	// exempt (and builtin skills are instructions-only, so they never reach here
	// with a runnable runtime anyway).
	if s.Runtime != "deno" {
		if s.Builtin {
			return "", fmt.Errorf("builtin skill %q is instructions-only and cannot be run", a.Skill)
		}
		return "", fmt.Errorf("skill %q has no runnable code; user skills must declare `runtime: deno` to run", a.Skill)
	}
	if !t.runner.Installed() {
		return "", fmt.Errorf("the Deno sandbox is not available; install Deno or set agent.deno_path")
	}

	entry := s.Entry
	if entry == "" {
		entry = "skill.js"
	}
	if strings.ContainsAny(entry, `/\`) || entry == ".." {
		return "", fmt.Errorf("skill entry must be a bare filename")
	}
	entryPath := filepath.Join(s.Dir, entry)
	if _, err := os.Stat(entryPath); err != nil {
		return "", fmt.Errorf("skill entry %q not found in skill %q", entry, a.Skill)
	}

	perms := tools.DenoPermissions{Read: []string{s.Dir}}
	if t.workspace != "" {
		perms.Read = append(perms.Read, t.workspace)
		if s.Write {
			perms.Write = append(perms.Write, t.workspace)
		}
	}
	perms.Net = s.Net

	return t.runner.Run(ctx, entryPath, a.Args, perms)
}
