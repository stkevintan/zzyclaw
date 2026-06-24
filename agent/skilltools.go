package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"zzy/agent/skill"
	"zzy/agent/tools"
)

// SkillTools builds the skill-management tools that let the model inspect and
// (un)load skills for the current session. They read the active session from the
// context, which the engine injects before executing any tool, and scope every
// operation to that user: each user sees only the shared builtin skills plus
// their own.
func SkillTools(mgr *skill.Manager) []tools.Tool {
	return []tools.Tool{
		&listSkillsTool{mgr: mgr},
		&loadSkillTool{mgr: mgr},
		&unloadSkillTool{mgr: mgr},
		&createSkillTool{mgr: mgr},
		&deleteSkillTool{mgr: mgr},
	}
}

// userIDFromContext returns the active user's ID, or "" when no session is set.
func userIDFromContext(ctx context.Context) string {
	if sess, ok := sessionFromContext(ctx); ok && sess != nil {
		return sess.UserID
	}
	return ""
}

type listSkillsTool struct{ mgr *skill.Manager }

func (t *listSkillsTool) Name() string { return "list_skills" }
func (t *listSkillsTool) Description() string {
	return "List all available skills with their descriptions and whether they are currently loaded into this conversation."
}
func (t *listSkillsTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *listSkillsTool) Dangerous(context.Context, json.RawMessage) bool { return false }
func (t *listSkillsTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	userID := userIDFromContext(ctx)
	_ = t.mgr.Reload(userID)
	sess, _ := sessionFromContext(ctx)
	skills := t.mgr.List(userID)
	if len(skills) == 0 {
		return "No skills are available yet.", nil
	}
	var b strings.Builder
	for _, s := range skills {
		loaded := ""
		if sess != nil {
			if _, ok := sess.ActiveSkills[s.Name]; ok {
				loaded = " [loaded]"
			}
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", s.Name, loaded, s.Description)
	}
	return strings.TrimSpace(b.String()), nil
}

type loadSkillTool struct{ mgr *skill.Manager }

func (t *loadSkillTool) Name() string { return "load_skill" }
func (t *loadSkillTool) Description() string {
	return "Load a skill by name so its full instructions become available in this conversation."
}
func (t *loadSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"The skill name to load."}},"required":["name"]}`)
}
func (t *loadSkillTool) Dangerous(context.Context, json.RawMessage) bool { return false }
func (t *loadSkillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	sess, ok := sessionFromContext(ctx)
	if !ok || sess == nil {
		return "", fmt.Errorf("no active session")
	}
	_ = t.mgr.Reload(sess.UserID)
	s, ok := t.mgr.Get(sess.UserID, a.Name)
	if !ok {
		return "", fmt.Errorf("unknown skill %q", a.Name)
	}
	sess.ActiveSkills[s.Name] = struct{}{}
	return fmt.Sprintf("Loaded skill %q. Its instructions are now active:\n\n%s", s.Name, s.Instructions), nil
}

type unloadSkillTool struct{ mgr *skill.Manager }

func (t *unloadSkillTool) Name() string { return "unload_skill" }
func (t *unloadSkillTool) Description() string {
	return "Unload a previously loaded skill to free up context."
}
func (t *unloadSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"The skill name to unload."}},"required":["name"]}`)
}
func (t *unloadSkillTool) Dangerous(context.Context, json.RawMessage) bool { return false }
func (t *unloadSkillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	sess, ok := sessionFromContext(ctx)
	if !ok || sess == nil {
		return "", fmt.Errorf("no active session")
	}
	delete(sess.ActiveSkills, a.Name)
	return fmt.Sprintf("Unloaded skill %q.", a.Name), nil
}

// createSkillTool persists a skill as a self-contained folder (SKILL.md plus an
// optional Deno entry file) in the calling user's own skills directory. It is
// the supported way to author skills, so the files reliably land in the registry
// rather than the workspace. Creating/updating a capability is approval-gated.
type createSkillTool struct{ mgr *skill.Manager }

func (t *createSkillTool) Name() string { return "create_skill" }
func (t *createSkillTool) Description() string {
	return "Create or update a skill as a self-contained folder in your skills directory. Provide the full SKILL.md markdown and, for an executable (runtime: deno) skill, the entry filename (e.g. skill.js) and its source code. The registry picks it up immediately. This is the only supported way to author skills; do not use write_file for skill files. Builtin skills cannot be overwritten."
}
func (t *createSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Skill name and folder name: lowercase letters, digits and hyphens."},"skill_md":{"type":"string","description":"Full SKILL.md contents, including the frontmatter block."},"entry_file":{"type":"string","description":"Optional Deno entry filename (skill.js, skill.ts or *.mjs). Required for executable skills."},"entry_code":{"type":"string","description":"Source code for entry_file. Required when entry_file is set."}},"required":["name","skill_md"]}`)
}
func (t *createSkillTool) Dangerous(context.Context, json.RawMessage) bool { return true }
func (t *createSkillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name      string `json:"name"`
		SkillMD   string `json:"skill_md"`
		EntryFile string `json:"entry_file"`
		EntryCode string `json:"entry_code"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	sess, ok := sessionFromContext(ctx)
	if !ok || sess == nil {
		return "", fmt.Errorf("no active session")
	}
	if err := t.mgr.Create(sess.UserID, a.Name, a.SkillMD, a.EntryFile, a.EntryCode); err != nil {
		return "", err
	}
	files := "SKILL.md"
	if a.EntryFile != "" {
		files += ", " + a.EntryFile
	}
	return fmt.Sprintf("Saved skill %q (%s). It is now available.", a.Name, files), nil
}

// deleteSkillTool removes one of the user's own skill folders. Builtin skills
// are protected. Deletion is destructive, so it is approval-gated.
type deleteSkillTool struct{ mgr *skill.Manager }

func (t *deleteSkillTool) Name() string { return "delete_skill" }
func (t *deleteSkillTool) Description() string {
	return "Delete one of your skills, removing its entire folder (SKILL.md and any entry file) from your skills directory. Builtin skills cannot be deleted."
}
func (t *deleteSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"The skill name to delete."}},"required":["name"]}`)
}
func (t *deleteSkillTool) Dangerous(context.Context, json.RawMessage) bool { return true }
func (t *deleteSkillTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	sess, ok := sessionFromContext(ctx)
	if !ok || sess == nil {
		return "", fmt.Errorf("no active session")
	}
	if err := t.mgr.Remove(sess.UserID, a.Name); err != nil {
		return "", err
	}
	delete(sess.ActiveSkills, a.Name)
	return fmt.Sprintf("Deleted skill %q.", a.Name), nil
}

// loadedSkillInstructions returns the instructions of all skills currently
// loaded in the session, sorted by name, for inclusion in the system prompt.
func loadedSkillInstructions(mgr *skill.Manager, sess *Session) []string {
	if sess == nil {
		return nil
	}
	names := make([]string, 0, len(sess.ActiveSkills))
	for name := range sess.ActiveSkills {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		if s, ok := mgr.Get(sess.UserID, name); ok {
			out = append(out, fmt.Sprintf("## Skill: %s\n%s", s.Name, s.Instructions))
		}
	}
	return out
}
