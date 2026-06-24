// Package skill implements a disk-backed registry of agent skills. A skill is a
// directory containing a SKILL.md file with YAML-ish frontmatter (name,
// description) followed by markdown instructions. Skills can be created at
// runtime (e.g. by the write-skill skill) and picked up via Reload without
// recompiling the program.
package skill

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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
	Write   bool     // when true, the skill may write to the workspace; default read-only

	// Builtin marks system-seeded skills (e.g. write-skill). It is derived from a
	// compiled-in allowlist, never from frontmatter, so an untrusted skill cannot
	// claim it. Only builtin skills are exempt from the deno-only rule for code.
	Builtin bool
}

// builtinSkills is the compiled-in set of system skills. Membership (not
// frontmatter) is the single source of truth for Skill.Builtin.
var builtinSkills = map[string]bool{
	"write-skill": true,
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
		s.Builtin = builtinSkills[s.Name]
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
	if builtinSkills[name] {
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
	if builtinSkills[name] {
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

// parse splits SKILL.md into frontmatter (name/description) and instructions.
// Frontmatter is delimited by a leading "---" / trailing "---" block; lines
// inside use "key: value". Everything after the closing delimiter is the body.
func parse(content string) *Skill {
	s := &Skill{}
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}

	idx := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		// Parse frontmatter until the closing delimiter.
		i := 1
		for ; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if line == "---" {
				i++
				break
			}
			key, val, found := strings.Cut(line, ":")
			if !found {
				continue
			}
			key = strings.TrimSpace(strings.ToLower(key))
			val = strings.TrimSpace(val)
			val = strings.Trim(val, `"'`)
			switch key {
			case "name":
				s.Name = val
			case "description":
				s.Description = val
			case "runtime":
				s.Runtime = strings.ToLower(val)
			case "entry":
				s.Entry = val
			case "net":
				s.Net = splitList(val)
			case "write":
				v := strings.ToLower(val)
				s.Write = v == "true" || v == "yes" || v == "1" || v == "workspace"
			}
		}
		idx = i
	}
	s.Instructions = strings.TrimSpace(strings.Join(lines[idx:], "\n"))
	if s.Description == "" {
		// Fall back to the first non-empty body line as a description.
		for _, l := range lines[idx:] {
			if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
				s.Description = t
				break
			}
		}
	}
	return s
}

// splitList parses a comma- or space-separated frontmatter value into a cleaned,
// lowercased list of tokens (used for the network host allowlist). The values
// "none" and "false" yield an empty list.
func splitList(val string) []string {
	val = strings.TrimSpace(strings.ToLower(val))
	if val == "" || val == "none" || val == "false" {
		return nil
	}
	fields := strings.FieldsFunc(val, func(r rune) bool {
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
