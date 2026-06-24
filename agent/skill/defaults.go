package skill

import "sort"

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
  hostnames. Accepts a comma/space-separated string or a YAML list:

   ` + "```" + `
   net:
     - example.com
     - api.example.org
   ` + "```" + `

  Omit for no network. Prefer scoping to exact hostnames; ` + "`net: \"*\"`" + ` grants
  access to every host and should be avoided unless truly unavoidable.

- ` + "`env: API_TOKEN, HOME`" + ` — grant read access to ONLY the listed environment
  variable names (case-sensitive). The sandbox otherwise hides the host
  environment entirely; declared variables are passed through with their host
  values. Accepts a comma/space-separated string or a YAML list. Omit for no
  environment access. There is no wildcard — list each variable explicitly.

   ` + "```" + `
   ---
   name: <skill-name>
   description: <one sentence describing when to use this skill>
   runtime: deno
   entry: skill.js
   net: api.example.com
   env: API_TOKEN
   write: true
   ---
   ` + "```" + `

Skills run inside the Deno sandbox, which enforces these permissions. A skill
that declares ` + "`write`" + `, ` + "`net`" + `, or ` + "`env`" + ` can modify the workspace, reach the
network, or read the named environment variables
within those limits, so running it asks the user for approval first (they may
reply "always" to remember that skill). A read-only, no-network skill runs
without prompting. Request the minimum: omit ` + "`write`/`net`/`env`" + ` unless the
skill truly needs them.

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

// builtinDocs holds the compiled-in builtin skills' SKILL.md content, keyed by
// name. Builtins ship inside the binary (imported via the Go module) and are
// served from memory by the Manager — they are never written to or scanned from
// disk.
var builtinDocs = map[string]string{
	"write-skill": writeSkillDoc,
}

// builtinSkillSet is the membership set derived from builtinDocs. Membership
// (not frontmatter) is the single source of truth for Skill.Builtin and guards
// builtin names against being overwritten or deleted on disk.
var builtinSkillSet = func() map[string]bool {
	m := make(map[string]bool, len(builtinDocs))
	for name := range builtinDocs {
		m[name] = true
	}
	return m
}()

// builtinSkills is the parsed, in-memory form of builtinDocs, built once at
// startup from the compiled-in documents.
var builtinSkills = func() map[string]*Skill {
	m := make(map[string]*Skill, len(builtinDocs))
	for name, doc := range builtinDocs {
		s := parse(doc)
		s.Name = name
		s.Builtin = true
		m[name] = s
	}
	return m
}()

// builtinList returns the compiled-in builtin skills sorted by name. Each entry
// is a shallow copy so callers can never mutate the shared in-memory originals.
func builtinList() []*Skill {
	out := make([]*Skill, 0, len(builtinSkills))
	for _, s := range builtinSkills {
		sCopy := *s
		out = append(out, &sCopy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
