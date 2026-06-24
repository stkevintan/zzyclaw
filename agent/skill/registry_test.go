package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseFrontmatter(t *testing.T) {
	doc := "---\nname: foo\ndescription: does a thing\n---\n# Title\n\nbody line"
	s := parse(doc)
	if s.Name != "foo" {
		t.Errorf("name = %q, want foo", s.Name)
	}
	if s.Description != "does a thing" {
		t.Errorf("description = %q", s.Description)
	}
	if s.Instructions == "" || s.Instructions[0] != '#' {
		t.Errorf("instructions should start with the body, got %q", s.Instructions)
	}
}

func TestParseNoFrontmatter(t *testing.T) {
	doc := "# Title\nsome description line"
	s := parse(doc)
	if s.Name != "" {
		t.Errorf("expected empty name, got %q", s.Name)
	}
	if s.Description != "some description line" {
		t.Errorf("expected fallback description, got %q", s.Description)
	}
}

func TestParseRuntimeFields(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: DENO\nentry: skill.ts\nnet: api.example.com, b.example.org\nwrite: true\n---\n# Greet\n"
	s := parse(doc)
	if s.Runtime != "deno" {
		t.Errorf("runtime = %q, want deno (lowercased)", s.Runtime)
	}
	if s.Entry != "skill.ts" {
		t.Errorf("entry = %q, want skill.ts", s.Entry)
	}
	if len(s.Net) != 2 || s.Net[0] != "api.example.com" || s.Net[1] != "b.example.org" {
		t.Errorf("net = %v, want [api.example.com b.example.org]", s.Net)
	}
	if !s.Write {
		t.Error("write = false, want true")
	}
}

func TestParseDefaultsNoElevatedPerms(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: deno\n---\n# Greet\n"
	s := parse(doc)
	if len(s.Net) != 0 {
		t.Errorf("net = %v, want empty (no network by default)", s.Net)
	}
	if s.Write {
		t.Error("write = true, want false (read-only by default)")
	}
}

func TestParseNetBlockSequence(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: deno\nnet:\n  - api.example.com\n  - \"b.example.org\"\nwrite: yes\n---\n# Greet\n"
	s := parse(doc)
	if len(s.Net) != 2 || s.Net[0] != "api.example.com" || s.Net[1] != "b.example.org" {
		t.Errorf("net = %v, want [api.example.com b.example.org]", s.Net)
	}
	if !s.Write {
		t.Error("write = false, want true (write: yes)")
	}
}

func TestParseNetWildcard(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: deno\nnet:\n  - \"*\"\n---\n# Greet\n"
	s := parse(doc)
	if len(s.Net) != 1 || s.Net[0] != "*" {
		t.Errorf("net = %v, want [*]", s.Net)
	}
}

func TestParseNetNoneIsEmpty(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: deno\nnet: none\n---\n# Greet\n"
	s := parse(doc)
	if len(s.Net) != 0 {
		t.Errorf("net = %v, want empty for \"none\"", s.Net)
	}
}

func TestParseEnvPreservesCase(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: deno\nenv: API_TOKEN, Home\n---\n# Greet\n"
	s := parse(doc)
	if len(s.Env) != 2 || s.Env[0] != "API_TOKEN" || s.Env[1] != "Home" {
		t.Errorf("env = %v, want [API_TOKEN Home] (case preserved)", s.Env)
	}
}

func TestParseEnvBlockSequence(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: deno\nenv:\n  - API_TOKEN\n  - \"PATH\"\n---\n# Greet\n"
	s := parse(doc)
	if len(s.Env) != 2 || s.Env[0] != "API_TOKEN" || s.Env[1] != "PATH" {
		t.Errorf("env = %v, want [API_TOKEN PATH]", s.Env)
	}
}

func TestParseEnvNoneIsEmpty(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: deno\nenv: none\n---\n# Greet\n"
	s := parse(doc)
	if len(s.Env) != 0 {
		t.Errorf("env = %v, want empty for \"none\"", s.Env)
	}
}

func TestParseEnvDeduplicates(t *testing.T) {
	doc := "---\nname: greet\ndescription: x\nruntime: deno\nenv: API_TOKEN, HOME, API_TOKEN\n---\n# Greet\n"
	s := parse(doc)
	if len(s.Env) != 2 || s.Env[0] != "API_TOKEN" || s.Env[1] != "HOME" {
		t.Errorf("env = %v, want deduped [API_TOKEN HOME]", s.Env)
	}
}

func TestCreateAndRemoveSkill(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	md := "---\nname: greet\ndescription: greet someone\nruntime: deno\nentry: skill.js\n---\n# Greet\n"
	if err := r.Create("greet", md, "skill.js", "console.log('hi')\n"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s, ok := r.Get("greet")
	if !ok {
		t.Fatal("expected greet skill after Create")
	}
	if s.Runtime != "deno" || s.Entry != "skill.js" {
		t.Errorf("unexpected parsed skill: runtime=%q entry=%q", s.Runtime, s.Entry)
	}
	// The entry file must live in the same folder as SKILL.md.
	if _, err := os.Stat(filepath.Join(dir, "greet", "skill.js")); err != nil {
		t.Errorf("entry file not written to skill folder: %v", err)
	}

	if err := r.Remove("greet"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := r.Get("greet"); ok {
		t.Error("greet should be gone after Remove")
	}
	if _, err := os.Stat(filepath.Join(dir, "greet")); !os.IsNotExist(err) {
		t.Error("skill folder should be deleted")
	}
}

func TestCreateRejectsBadNamesAndEntries(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	md := "---\nname: x\ndescription: y\n---\n# X\n"
	if err := r.Create("../escape", md, "", ""); err == nil {
		t.Error("expected error for path-escaping name")
	}
	if err := r.Create("Bad Name", md, "", ""); err == nil {
		t.Error("expected error for invalid name characters")
	}
	if err := r.Create("ok", md, "skill.sh", "echo hi"); err == nil {
		t.Error("expected error for non-JS/TS entry file")
	}
	if err := r.Create("ok", md, "../evil.js", "x"); err == nil {
		t.Error("expected error for path-escaping entry file")
	}
}

func TestBuiltinSkillProtected(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	md := "---\nname: write-skill\ndescription: y\n---\n# X\n"
	if err := r.Create("write-skill", md, "", ""); err == nil {
		t.Error("expected error overwriting builtin skill")
	}
	if err := r.Remove("write-skill"); err == nil {
		t.Error("expected error deleting builtin skill")
	}
}
