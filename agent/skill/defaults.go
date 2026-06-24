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

You can extend yourself by creating new skills on disk. A skill is a directory
under the skills folder containing a ` + "`SKILL.md`" + ` file.

## How to create a skill

1. Decide a short, lowercase, hyphenated skill name (e.g. ` + "`weather-report`" + `).
2. Use the ` + "`write_file`" + ` tool to create ` + "`<skill-name>/SKILL.md`" + ` (relative to
   the skills directory; it sits next to this skill) with this exact structure:

   ` + "```" + `
   ---
   name: <skill-name>
   description: <one sentence describing when to use this skill>
   ---

   # <Skill Title>

   <Clear, step-by-step instructions the agent should follow when this skill is loaded.>
   ` + "```" + `

3. If the skill needs a reusable command, create a script with ` + "`write_file`" + ` in the
   scripts directory and document how to call it with the ` + "`run_script`" + ` tool.
4. If a Python script needs third-party packages, install them first with the
   ` + "`pip_install`" + ` tool (e.g. ` + "`requests`, `beautifulsoup4`" + `). Only the Python
   standard library is available by default.
5. After writing the files, tell the user the skill was created and that it is now
   available (the registry reloads automatically when listing or loading skills).

## Guidance

- Keep the description specific so future-you knows exactly when to load it.
- Put only durable, reusable instructions in a skill — not one-off task details.
- Never overwrite this write-skill file.
`

// Seed writes the built-in write-skill into the registry directory if it is not
// already present, and ensures the scripts directory exists. It then reloads.
func (r *Registry) Seed(scriptsDir string) error {
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return err
	}
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
