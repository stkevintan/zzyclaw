package skill

import (
	"path/filepath"
	"testing"
)

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

func hasSkill(skills []*Skill, name string) bool {
	for _, s := range skills {
		if s.Name == name {
			return true
		}
	}
	return false
}
