package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"zzy/agent/skill"
	"zzy/agent/tools"
)

// runSkillTool executes a skill's code inside the Deno sandbox. Only skills that
// declare `runtime: deno` are runnable. By default a skill runs read-only with
// access to its own directory and the calling user's workspace and no network,
// which needs no approval. A skill may opt into workspace writes (`write: true`)
// or network access (`net: <hosts>`); those elevated runs are treated as
// dangerous and go through the approval (and owner) gate. Approving such a run
// with "always" is remembered per skill and capability set, so re-prompting only
// happens if the skill later changes the write/network access it declares.
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
	return "Run a sandboxed skill's code (skills with `runtime: deno`). The code runs in the Deno sandbox: by default it may only read its own directory and the workspace, with no network. Skills that declare `write: true` or `net: <hosts>` get those capabilities and require approval (you can reply \"always\" once to remember the skill). The skill's stdout is returned. Use list_skills to discover skills."
}
func (t *runSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string","description":"Name of the skill to run."},"args":{"type":"array","items":{"type":"string"},"description":"Arguments passed to the skill program."}},"required":["skill"]}`)
}

// Dangerous returns true when the targeted skill requests elevated capabilities
// (workspace write or network), so the engine prompts for approval first.
// Read-only, no-network skills run without a prompt. The skill is resolved for
// the active user so each user is gated only by their own skills' declarations.
func (t *runSkillTool) Dangerous(ctx context.Context, args json.RawMessage) bool {
	s, ok := t.resolveSkill(ctx, args)
	return ok && skillElevated(s)
}

// GrantScope lets an elevated skill run be remembered with "always". The grant is
// scoped to the skill name AND the exact write/network capabilities it declares,
// so if the skill later widens that access (e.g. adds a host or turns on write)
// the key changes and the user is prompted again — a remembered approval can
// never silently cover newly-declared capabilities.
func (t *runSkillTool) GrantScope(ctx context.Context, args json.RawMessage) (key, label string, ok bool) {
	s, ok := t.resolveSkill(ctx, args)
	if !ok || !skillElevated(s) {
		return "", "", false
	}
	write := "0"
	if s.Write {
		write = "1"
	}
	net := append([]string(nil), s.Net...)
	sort.Strings(net)
	key = fmt.Sprintf("run_skill:%s:w=%s;n=%s", s.Name, write, strings.Join(net, ","))
	return key, fmt.Sprintf("running the skill %q with its current access", s.Name), true
}

// resolveSkill loads the skill named in args for the active user. It returns
// ok=false for malformed args, unknown skills, or non-deno (instructions-only)
// skills, none of which are runnable through the sandbox.
func (t *runSkillTool) resolveSkill(ctx context.Context, args json.RawMessage) (*skill.Skill, bool) {
	var a struct {
		Skill string `json:"skill"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.Skill == "" {
		return nil, false
	}
	userID := userIDFromContext(ctx)
	s, ok := t.mgr.Get(userID, a.Skill)
	if !ok || s.Runtime != "deno" {
		return nil, false
	}
	return s, true
}

// skillElevated reports whether a skill requests capabilities beyond the
// read-only default (workspace write or network), which is what makes a run
// dangerous and subject to approval.
func skillElevated(s *skill.Skill) bool {
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
