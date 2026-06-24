package skill

import (
	"os"
	"path/filepath"
)

// writeSkillDoc is the built-in skill that teaches the agent how to author new
// skills (and helper scripts) on disk in response to a user's request.
const writeSkillDoc = `---
name: write-skill
description: Create a new agent skill from the user's intent. Use this whenever the user asks to teach, add, or build a new capability/skill for the agent.
---

# Write Skill

You can extend yourself by creating new skills. Each skill is a self-contained
folder in the dedicated skills directory, holding a ` + "`SKILL.md`" + ` file and, for
skills that run code, a Deno entry file (e.g. ` + "`skill.js`" + `). Manage skills with
the ` + "`create_skill`" + ` and ` + "`delete_skill`" + ` tools — never use ` + "`write_file`" + ` for skill
files, and never edit this skills directory by hand.

## How to create a skill

1. Decide a short, lowercase, hyphenated skill name (e.g. ` + "`weather-report`" + `).
2. Call ` + "`create_skill`" + ` with the ` + "`name`" + ` and the full ` + "`skill_md`" + ` contents. For an
   instructions-only skill, ` + "`skill_md`" + ` uses this structure:

   ` + "```" + `
   ---
   name: <skill-name>
   description: <one sentence describing when to use this skill>
   ---

   # <Skill Title>

   <Clear, step-by-step instructions the agent should follow when this skill is loaded.>
   ` + "```" + `

3. If the skill needs to run code, it MUST be a **Deno** skill. Set ` + "`runtime: deno`" + `
   in the frontmatter and pass the entry file to ` + "`create_skill`" + ` via ` + "`entry_file`" + `
   (e.g. ` + "`skill.js`" + `) and ` + "`entry_code`" + ` (its source). The entry file is saved
   inside the same skill folder. The agent runs it with the ` + "`run_skill`" + ` tool; the
   code runs in the Deno sandbox and returns results by printing to stdout.
   Shell/native (` + "`.sh`/.py`" + `) skills are NOT allowed — always use Deno.

   ` + "```" + `
   ---
   name: <skill-name>
   description: <one sentence describing when to use this skill>
   runtime: deno
   entry: skill.js
   ---

   # <Skill Title>

   <Instructions. Explain what arguments run_skill should pass and what the
   skill prints.>
   ` + "```" + `

   The entry file may be JavaScript (` + "`skill.js`" + `, the default) or TypeScript
   (` + "`skill.ts`" + ` / ` + "`*.mjs`" + `); set ` + "`entry:`" + ` in the frontmatter to match ` + "`entry_file`" + `.

## Sandbox permissions (default deny)

By default a Deno skill runs with the LEAST privilege:

- **Read-only** access to its own skill folder and the workspace.
- **No** workspace writes.
- **No** network.

Read its command-line arguments via ` + "`Deno.args`" + ` and read files with the
standard Deno APIs (the skill folder is the process working directory).

Opt into more only when the skill truly needs it, using frontmatter:

- ` + "`write: true`" + ` — grant write access to the workspace directory. Use this only
  when the skill must persist files.
- ` + "`net: example.com, api.example.org`" + ` — grant network access to ONLY the listed
  hostnames (comma-separated). Omit for no network. Never use a wildcard.

   ` + "```" + `
   ---
   name: <skill-name>
   description: <one sentence describing when to use this skill>
   runtime: deno
   entry: skill.js
   net: api.example.com
   write: true
   ---
   ` + "```" + `

Skills that request ` + "`write`" + ` or ` + "`net`" + ` require the user's approval each run.
Read-only, no-network skills run without a prompt, so keep the permissions
minimal.

4. After ` + "`create_skill`" + ` succeeds, tell the user the skill was created and that it
   is now available (the registry reloads automatically).

## Managing skills

- To remove a skill the user no longer wants, call ` + "`delete_skill`" + ` with its name;
  this deletes the whole skill folder. Builtin skills cannot be deleted.
- To update a skill, call ` + "`create_skill`" + ` again with the same name (it overwrites
  the folder's files).
- Use ` + "`list_skills`" + ` to see what already exists before creating a new one.

## Guidance

- ALL executable skills use ` + "`runtime: deno`" + `. There is no shell/native skill tier.
- Request the minimum permissions: no ` + "`net`" + ` and no ` + "`write`" + ` unless essential, and
  scope ` + "`net`" + ` to the exact hostnames needed.
- Keep the description specific so future-you knows exactly when to load it.
- Put only durable, reusable instructions in a skill — not one-off task details.
- Never overwrite or delete this write-skill skill.
`

// Seed writes the built-in write-skill into the registry directory if it is not
// already present, then reloads.
func (r *Registry) Seed() error {
	dir := filepath.Join(r.dir, "write-skill")
	path := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(writeSkillDoc), 0o644); err != nil {
			return err
		}
	}
	return r.Reload()
}
