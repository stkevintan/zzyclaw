package agent

import (
	"context"
	"testing"
)

func TestUserMemoryAddListAndDedup(t *testing.T) {
	m := NewStoreUserMemory(NewInMemoryStore())
	ctx := context.Background()

	a, err := m.Add(ctx, "u1", "Prefers dark mode")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if a.ID == "" || a.Text != "Prefers dark mode" {
		t.Fatalf("unexpected fact: %+v", a)
	}
	if _, err := m.Add(ctx, "u1", "Uses vim keybindings"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Exact (case-insensitive) duplicate returns the existing fact, no growth.
	dup, err := m.Add(ctx, "u1", "prefers DARK mode")
	if err != nil {
		t.Fatalf("Add dup: %v", err)
	}
	if dup.ID != a.ID {
		t.Errorf("duplicate created a new fact: %s != %s", dup.ID, a.ID)
	}
	facts, err := m.List(ctx, "u1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("List returned %d facts, want 2", len(facts))
	}
}

func TestUserMemoryAddRejectsBlank(t *testing.T) {
	m := NewStoreUserMemory(NewInMemoryStore())
	if _, err := m.Add(context.Background(), "u1", "   "); err == nil {
		t.Error("expected error for blank fact")
	}
}

func TestUserMemorySearchKeyword(t *testing.T) {
	m := NewStoreUserMemory(NewInMemoryStore())
	ctx := context.Background()
	_, _ = m.Add(ctx, "u1", "Allergic to peanuts")
	_, _ = m.Add(ctx, "u1", "Works as a math teacher")
	_, _ = m.Add(ctx, "u1", "Lives in Shanghai")

	// v1 ranking is keyword overlap (no embeddings), so the query must share a
	// literal word with the fact.
	got, err := m.Search(ctx, "u1", "peanuts please", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) == 0 || got[0].Text != "Allergic to peanuts" {
		t.Fatalf("Search top result = %+v, want the peanut allergy fact", got)
	}
}

func TestUserMemorySearchEmptyReturnsRecent(t *testing.T) {
	m := NewStoreUserMemory(NewInMemoryStore())
	ctx := context.Background()
	_, _ = m.Add(ctx, "u1", "first")
	last, _ := m.Add(ctx, "u1", "second")

	got, err := m.Search(ctx, "u1", "", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].ID != last.ID {
		t.Fatalf("empty-query search = %+v, want most recent (%s)", got, last.ID)
	}
}

func TestUserMemoryDelete(t *testing.T) {
	m := NewStoreUserMemory(NewInMemoryStore())
	ctx := context.Background()
	f, _ := m.Add(ctx, "u1", "temporary note")

	found, err := m.Delete(ctx, "u1", f.ID)
	if err != nil || !found {
		t.Fatalf("Delete = (%v, %v), want (true, nil)", found, err)
	}
	if found, _ := m.Delete(ctx, "u1", f.ID); found {
		t.Error("second Delete reported found for an already-removed fact")
	}
	if facts, _ := m.List(ctx, "u1"); len(facts) != 0 {
		t.Errorf("List after delete = %d facts, want 0", len(facts))
	}
}

func TestUserMemoryIsolatedPerUser(t *testing.T) {
	m := NewStoreUserMemory(NewInMemoryStore())
	ctx := context.Background()
	_, _ = m.Add(ctx, "alice", "alice secret")

	bob, err := m.List(ctx, "bob")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(bob) != 0 {
		t.Errorf("bob can see %d of alice's facts, want 0", len(bob))
	}
}

func TestUserMemoryPersistsThroughStore(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	// First instance writes; a second instance over the same store must read it
	// back, proving facts are persisted (not just cached in the first instance).
	_, _ = NewStoreUserMemory(store).Add(ctx, "u1", "durable fact")

	facts, err := NewStoreUserMemory(store).List(ctx, "u1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 1 || facts[0].Text != "durable fact" {
		t.Fatalf("reloaded facts = %+v, want the durable fact", facts)
	}
}
