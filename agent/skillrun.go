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
// declare `runtime: deno` are runnable. A skill runs as an isolated Deno
// subprocess: it may read its own directory and the calling user's workspace,
// and gets only the write (`write: true`) and network (`net: <hosts>`) access its
// frontmatter declares. Because the sandbox enforces those limits and a skill
// cannot invoke the agent's tools or escape the sandbox, running one is not
// treated as dangerous and never prompts for approval.
type runSkillTool struct {
	mgr       *skill.Manager
	runner    *tools.DenoRunner
	workspace string
}

// RunSkillTool builds the run_skill tool. runner executes the Deno sandbox;
// workspace is the directory granted to skills (read-only by default).
func RunSkillTool(mgr *skill.Manager, runner *tools.DenoRunner, workspace string) tools.Tool {
	return &runSkillTool{mgr: mgr, runner: runner, workspace: workspace}
}

func (t *runSkillTool) Name() string { return "run_skill" }
func (t *runSkillTool) Description() string {
	return "Run a sandboxed skill's code (skills with `runtime: deno`). The code runs in the Deno sandbox: it may read its own directory and the workspace, and gets only the write/network access the skill declares in its frontmatter. Running a skill does not require approval because the sandbox enforces these limits. The skill's stdout is returned. Use list_skills to discover skills."
}
func (t *runSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","description":"Name of the skill to run."},"args":{"type":"array","items":{"type":"string"},"description":"Arguments passed to the skill program."}},"required":["skill"]}`)
}

// Dangerous always returns false: a skill runs as an isolated Deno subprocess
// whose only capabilities are the sandbox permissions Deno enforces (read its
// own dir and the calling user's workspace, plus any write/net the skill author
// declared in frontmatter). A skill cannot invoke the agent's tools or escape
// the sandbox, so running one never requires approval. The declared write/net
// grants are still applied to the Deno process in Execute.
func (t *runSkillTool) Dangerous(context.Context, json.RawMessage) bool { return false }

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
	userID := userIDFromContext(ctx)
	_ = t.mgr.Reload(userID)
	s, ok := t.mgr.Get(userID, a.Skill)
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

	// Deno is launched with its working directory set to the skill dir, so any
	// relative permission path would be resolved against it. Grant absolute paths
	// so the workspace and skill dir are always located correctly.
	absDir, err := filepath.Abs(s.Dir)
	if err != nil {
		return "", fmt.Errorf("absolute skill dir: %w", err)
	}
	perms := tools.DenoPermissions{Read: []string{absDir}}
	if t.workspace != "" {
		// Scope the skill's workspace access to the calling user's own
		// subdirectory so one user's skill run cannot read or write another
		// user's files.
		userWorkspace, err := tools.UserWorkspace(t.workspace, userID)
		if err != nil {
			return "", fmt.Errorf("user workspace: %w", err)
		}
		absWorkspace, err := filepath.Abs(userWorkspace)
		if err != nil {
			return "", fmt.Errorf("absolute workspace dir: %w", err)
		}
		perms.Read = append(perms.Read, absWorkspace)
		if s.Write {
			perms.Write = append(perms.Write, absWorkspace)
		}
	}
	perms.Net = s.Net

	return t.runner.Run(ctx, entryPath, a.Args, perms)
}
