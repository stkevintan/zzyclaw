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
