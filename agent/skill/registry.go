// Package skill implements a disk-backed registry of agent skills. A skill is a
// directory containing a SKILL.md file with YAML-ish frontmatter (name,
// description) followed by markdown instructions. Skills can be created at
// runtime (e.g. by the write-skill skill) and picked up via Reload without
// recompiling the program.
package skill

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	yaml "go.yaml.in/yaml/v3"
)

// Skill is a parsed skill definition loaded from disk.
type Skill struct {
	Name         string // unique identifier (directory name / frontmatter name)
	Description  string // short summary used to decide when to load the skill
	Instructions string // full markdown body injected into the system prompt when loaded
	Dir          string // absolute path to the skill directory

	// Execution: user-added skills that run code MUST use the Deno sandbox.
	Runtime string   // "deno" for executable skills; empty = instructions-only skill
	Entry   string   // entry source file run by Deno (default "skill.js"; .ts also allowed)
	Net     []string // network hosts the skill may reach (Deno --allow-net); empty = no network
	Env     []string // environment variable names the skill may read (Deno --allow-env); empty = none
	Write   bool     // when true, the skill may write to the workspace; default read-only

	// Builtin marks system skills (e.g. write-skill). It is derived from a
	// compiled-in allowlist, never from frontmatter, so an untrusted skill cannot
	// claim it. Only builtin skills are exempt from the deno-only rule for code.
	Builtin bool
}

// Registry holds the skills discovered under a root directory.
type Registry struct {
	dir string

	mu     sync.RWMutex
	skills map[string]*Skill
}

// NewRegistry creates a registry rooted at dir and performs an initial scan.
func NewRegistry(dir string) (*Registry, error) {
	r := &Registry{dir: dir, skills: make(map[string]*Skill)}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("skill: create dir: %w", err)
	}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Dir returns the registry's root directory.
func (r *Registry) Dir() string { return r.dir }

// Reload rescans the skills directory from disk, replacing the in-memory set.
func (r *Registry) Reload() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return fmt.Errorf("skill: read dir: %w", err)
	}
	loaded := make(map[string]*Skill)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(r.dir, e.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue // not a skill directory
		}
		s := parse(string(data))
		if s.Name == "" {
			s.Name = e.Name()
		}
		s.Dir = filepath.Join(r.dir, e.Name())
		s.Builtin = builtinSkillSet[s.Name]
		loaded[s.Name] = s
	}
	r.mu.Lock()
	r.skills = loaded
	r.mu.Unlock()
	return nil
}

// List returns all known skills sorted by name.
func (r *Registry) List() []*Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Skill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the skill registered under name.
func (r *Registry) Get(name string) (*Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// skillNameRe constrains skill names to a safe, single-segment slug so a skill
// folder can never escape the registry directory or collide with path syntax.
var skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidName reports whether name is an acceptable skill identifier (and folder
// name): lowercase letters, digits and hyphens.
func ValidName(name string) bool { return skillNameRe.MatchString(name) }

// validEntry ensures an entry filename is a bare JS/TS source file that lives
// directly inside the skill folder.
func validEntry(entry string) error {
	if entry == "" || entry == ".." || strings.ContainsAny(entry, `/\`) {
		return fmt.Errorf("entry file must be a bare filename inside the skill folder")
	}
	switch filepath.Ext(entry) {
	case ".js", ".ts", ".mjs":
		return nil
	default:
		return fmt.Errorf("entry file must end in .js, .ts or .mjs")
	}
}

// Create writes a skill as a self-contained folder <dir>/<name>/ containing
// SKILL.md and, when entryFile/entryCode are provided, the Deno entry source.
// It creates or updates a user skill; builtin skills cannot be overwritten. The
// registry is reloaded on success so the skill is immediately available.
func (r *Registry) Create(name, skillMD, entryFile, entryCode string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid skill name %q: use lowercase letters, digits and hyphens", name)
	}
	if builtinSkillSet[name] {
		return fmt.Errorf("%q is a builtin skill and cannot be overwritten", name)
	}
	if strings.TrimSpace(skillMD) == "" {
		return fmt.Errorf("SKILL.md content must not be empty")
	}
	if entryFile != "" {
		if err := validEntry(entryFile); err != nil {
			return err
		}
		if strings.TrimSpace(entryCode) == "" {
			return fmt.Errorf("entry_code must not be empty when entry_file is set")
		}
	}
	dir := filepath.Join(r.dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md: %w", err)
	}
	if entryFile != "" {
		if err := os.WriteFile(filepath.Join(dir, entryFile), []byte(entryCode), 0o644); err != nil {
			return fmt.Errorf("write entry file: %w", err)
		}
	}
	return r.Reload()
}

// Remove deletes a user skill's folder (markdown plus any entry source).
// Builtin skills cannot be removed. The registry is reloaded on success.
func (r *Registry) Remove(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid skill name %q", name)
	}
	if builtinSkillSet[name] {
		return fmt.Errorf("%q is a builtin skill and cannot be deleted", name)
	}
	dir := filepath.Join(r.dir, name)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return fmt.Errorf("skill %q does not exist", name)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete skill: %w", err)
	}
	return r.Reload()
}

// frontmatter is the YAML header of a SKILL.md file. The custom types let net
// and write accept either a scalar (net: "*", net: "a.com, b.com", write: true)
// or a YAML sequence/bool, so both styles parse identically.
type frontmatter struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Runtime     string    `yaml:"runtime"`
	Entry       string    `yaml:"entry"`
	Net         netList   `yaml:"net"`
	Env         envList   `yaml:"env"`
	Write       writeFlag `yaml:"write"`
}

// netList accepts either a YAML sequence of hosts or a scalar string
// ("*", "none", "false", or a comma/space-separated list).
type netList []string

func (n *netList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.SequenceNode:
		// Reuse splitList per item so each entry is trimmed/lowercased and the
		// "none"/"false" sentinels are filtered, matching the scalar form.
		var out []string
		for _, item := range value.Content {
			out = append(out, splitList(item.Value)...)
		}
		*n = out
	default:
		*n = splitList(value.Value)
	}
	return nil
}

// envList accepts either a YAML sequence of variable names or a scalar string
// (comma/space-separated). Unlike netList it preserves case, because environment
// variable names are case-sensitive (e.g. PATH, HOME).
type envList []string

func (e *envList) UnmarshalYAML(value *yaml.Node) error {
	var raw []string
	switch value.Kind {
	case yaml.SequenceNode:
		for _, item := range value.Content {
			raw = append(raw, splitListCase(item.Value)...)
		}
	default:
		raw = splitListCase(value.Value)
	}
	// Deduplicate (preserving order) so the grant fingerprint stays stable and
	// Deno isn't handed redundant --allow-env entries.
	seen := make(map[string]bool, len(raw))
	var out []string
	for _, s := range raw {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	*e = out
	return nil
}

// writeFlag accepts a YAML bool or a string ("true"/"yes"/"1"/"workspace").
type writeFlag bool

func (w *writeFlag) UnmarshalYAML(value *yaml.Node) error {
	v := strings.ToLower(strings.TrimSpace(value.Value))
	*w = writeFlag(v == "true" || v == "yes" || v == "1" || v == "workspace")
	return nil
}

// parse splits SKILL.md into frontmatter (name/description/permissions) and
// instructions. Frontmatter is a leading "---" / trailing "---" block parsed as
// YAML; everything after the closing delimiter is the markdown body.
func parse(content string) *Skill {
	s := &Skill{}
	front, body := splitFrontmatter(content)
	if front != "" {
		var fm frontmatter
		if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
			// Surface malformed frontmatter rather than silently degrading to an
			// instructions-only skill (e.g. a bad runtime/net line being dropped).
			slog.Error("skill: parse frontmatter", "error", err)
		} else {
			s.Name = strings.TrimSpace(fm.Name)
			s.Description = strings.TrimSpace(fm.Description)
			s.Runtime = strings.ToLower(strings.TrimSpace(fm.Runtime))
			s.Entry = strings.TrimSpace(fm.Entry)
			s.Net = fm.Net
			s.Env = fm.Env
			s.Write = bool(fm.Write)
		}
	}
	s.Instructions = strings.TrimSpace(body)
	if s.Description == "" {
		// Fall back to the first non-empty, non-heading body line.
		for _, l := range strings.Split(s.Instructions, "\n") {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
				s.Description = t
				break
			}
		}
	}
	return s
}

// splitFrontmatter separates a leading "---"-delimited YAML block from the body.
// When there is no well-formed frontmatter block it returns the whole content as
// the body.
func splitFrontmatter(content string) (front, body string) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", content
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n")
		}
	}
	return "", content
}

// splitList parses a comma- or space-separated frontmatter value into a cleaned,
// lowercased list of tokens (used for the network host allowlist). The values
// "none" and "false" yield an empty list.
func splitList(val string) []string {
	out := splitListCase(val)
	for i, s := range out {
		out[i] = strings.ToLower(s)
	}
	return out
}

// splitListCase is splitList without lowercasing, for case-sensitive values such
// as environment variable names. The "none"/"false" sentinels (matched
// case-insensitively) still yield an empty list.
func splitListCase(val string) []string {
	trimmed := strings.TrimSpace(val)
	if lower := strings.ToLower(trimmed); lower == "" || lower == "none" || lower == "false" {
		return nil
	}
	fields := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
