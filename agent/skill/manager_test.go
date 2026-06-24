package skill

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuiltinsCompiledInNotOnDisk verifies that builtin skills are served from
// memory (compiled into the binary) and never written to the shared skills dir.
func TestBuiltinsCompiledInNotOnDisk(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "global")

	mgr, err := NewManager(globalDir, nil)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	// The builtin is resolvable and marked Builtin...
	s, ok := mgr.Get("", "write-skill")
	if !ok {
		t.Fatal("expected builtin write-skill to be served from memory")
	}
	if !s.Builtin || s.Instructions == "" {
		t.Errorf("write-skill should be a non-empty builtin, got builtin=%v", s.Builtin)
	}

	// ...but nothing was seeded to disk.
	if _, err := os.Stat(filepath.Join(globalDir, "write-skill")); !os.IsNotExist(err) {
		t.Error("write-skill must not be written to the shared skills directory")
	}
}

// TestManagerPerUserIsolation verifies that a skill created by one user is
// invisible to and unusable by another, while builtin skills are shared.
func TestManagerPerUserIsolation(t *testing.T) {
	base := t.TempDir()
	globalDir := filepath.Join(base, "global")
	userDir := func(userID string) (string, error) {
		return filepath.Join(base, "users", userID, "skills"), nil
	}

	mgr, err := NewManager(globalDir, userDir)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	// Alice creates a private skill.
	if err := mgr.Create("alice", "secret", "---\nname: secret\ndescription: alice only\n---\n# Secret\n", "", ""); err != nil {
		t.Fatalf("alice create: %v", err)
	}

	// Alice can see and resolve it.
	if _, ok := mgr.Get("alice", "secret"); !ok {
		t.Fatal("alice should see her own skill")
	}
	if !hasSkill(mgr.List("alice"), "secret") {
		t.Fatal("alice's list should include her skill")
	}

	// Bob cannot see or resolve Alice's skill.
	if _, ok := mgr.Get("bob", "secret"); ok {
		t.Fatal("bob must not resolve alice's skill")
	}
	if hasSkill(mgr.List("bob"), "secret") {
		t.Fatal("bob's list must not include alice's skill")
	}

	// Bob can create a same-named skill that is independent of Alice's.
	if err := mgr.Create("bob", "secret", "---\nname: secret\ndescription: bob only\n---\n# BobSecret\n", "", ""); err != nil {
		t.Fatalf("bob create: %v", err)
	}
	as, _ := mgr.Get("alice", "secret")
	bs, _ := mgr.Get("bob", "secret")
	if as.Description == bs.Description {
		t.Fatalf("each user's same-named skill must be independent, got %q for both", as.Description)
	}

	// The builtin write-skill is shared and visible to every user.
	if _, ok := mgr.Get("alice", "write-skill"); !ok {
		t.Fatal("alice should see the builtin write-skill")
	}
	if _, ok := mgr.Get("bob", "write-skill"); !ok {
		t.Fatal("bob should see the builtin write-skill")
	}

	// Builtins cannot be created or deleted by a user.
	if err := mgr.Create("alice", "write-skill", "---\nname: write-skill\ndescription: hijack\n---\n# X\n", "", ""); err == nil {
		t.Fatal("creating a builtin-named skill must be rejected")
	}
	if err := mgr.Remove("alice", "write-skill"); err == nil {
		t.Fatal("deleting a builtin skill must be rejected")
	}

	// A user context is required to create skills.
	if err := mgr.Create("", "anon", "---\nname: anon\ndescription: x\n---\n# X\n", "", ""); err == nil {
		t.Fatal("creating a skill without a user must be rejected")
	}
}

// TestSharedSkillVisibleToAllUsers verifies that a skill written to the shared
// registry is resolvable and listed for every user, and can be removed again.
func TestSharedSkillVisibleToAllUsers(t *testing.T) {
	base := t.TempDir()
	mgr, err := NewManager(filepath.Join(base, "global"), func(userID string) (string, error) {
		return filepath.Join(base, "users", userID, "skills"), nil
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	if err := mgr.CreateShared("team", "---\nname: team\ndescription: shared\n---\n# Team\n", "", ""); err != nil {
		t.Fatalf("create shared: %v", err)
	}

	for _, u := range []string{"alice", "bob", ""} {
		if _, ok := mgr.Get(u, "team"); !ok {
			t.Fatalf("user %q should resolve the shared skill", u)
		}
		if !hasSkill(mgr.List(u), "team") {
			t.Fatalf("user %q list should include the shared skill", u)
		}
	}

	// Builtins are still protected on the shared path.
	if err := mgr.CreateShared("write-skill", "---\nname: write-skill\ndescription: x\n---\n# X\n", "", ""); err == nil {
		t.Fatal("overwriting a builtin via the shared registry must be rejected")
	}

	if err := mgr.RemoveShared("team"); err != nil {
		t.Fatalf("remove shared: %v", err)
	}
	if _, ok := mgr.Get("alice", "team"); ok {
		t.Fatal("shared skill should be gone after removal")
	}
}

// TestPrivateSkillShadowsShared verifies that a user's private skill takes
// precedence over a shared skill of the same name (most-specific layer wins),
// while other users still see the shared one.
func TestPrivateSkillShadowsShared(t *testing.T) {
	base := t.TempDir()
	mgr, err := NewManager(filepath.Join(base, "global"), func(userID string) (string, error) {
		return filepath.Join(base, "users", userID, "skills"), nil
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	if err := mgr.CreateShared("notes", "---\nname: notes\ndescription: shared notes\n---\n# Shared\n", "", ""); err != nil {
		t.Fatalf("create shared: %v", err)
	}
	if err := mgr.Create("alice", "notes", "---\nname: notes\ndescription: alice notes\n---\n# Alice\n", "", ""); err != nil {
		t.Fatalf("alice create: %v", err)
	}

	// Alice resolves her own version.
	if s, ok := mgr.Get("alice", "notes"); !ok || s.Description != "alice notes" {
		t.Fatalf("alice should see her private notes, got %+v ok=%v", s, ok)
	}
	// Bob still resolves the shared version.
	if s, ok := mgr.Get("bob", "notes"); !ok || s.Description != "shared notes" {
		t.Fatalf("bob should see the shared notes, got %+v ok=%v", s, ok)
	}

	// Alice's list contains the private one exactly once.
	count, desc := 0, ""
	for _, s := range mgr.List("alice") {
		if s.Name == "notes" {
			count++
			desc = s.Description
		}
	}
	if count != 1 || desc != "alice notes" {
		t.Fatalf("alice list should contain her notes once, got count=%d desc=%q", count, desc)
	}
}

// TestBuiltinGetReturnsCopy verifies that mutating a skill returned by Get does
// not corrupt the shared compiled-in builtin.
func TestBuiltinGetReturnsCopy(t *testing.T) {
	base := t.TempDir()
	mgr, err := NewManager(filepath.Join(base, "global"), nil)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	s, ok := mgr.Get("", "write-skill")
	if !ok {
		t.Fatal("expected builtin write-skill")
	}
	original := s.Instructions
	s.Instructions = "tampered"
	if again, _ := mgr.Get("", "write-skill"); again.Instructions != original {
		t.Fatal("mutating a returned builtin must not affect the shared original")
	}
}

func hasSkill(skills []*Skill, name string) bool {
	for _, s := range skills {
		if s.Name == name {
			return true
		}
	}
	return false
}
