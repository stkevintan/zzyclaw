package skill

import "testing"

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

func TestSeedAndReload(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Seed(t.TempDir()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if _, ok := r.Get("write-skill"); !ok {
		t.Fatal("expected write-skill to be seeded")
	}
}
