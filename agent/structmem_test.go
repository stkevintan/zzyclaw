package agent

import (
	"context"
	"testing"
)

func TestStructMemUpsertMergeAndCategory(t *testing.T) {
	m := NewStoreStructuralMemory(NewInMemoryStore(), fakeEmbedder{})
	ctx := context.Background()

	if _, err := m.Upsert(ctx, "u1", CategoryPersonal, "prefers dark mode", "uses a dark theme"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Near-identical text in the same category merges (no growth).
	if _, err := m.Upsert(ctx, "u1", CategoryPersonal, "prefers dark mode", "always dark"); err != nil {
		t.Fatalf("upsert dup: %v", err)
	}
	// Same text, different category is a separate entry.
	if _, err := m.Upsert(ctx, "u1", CategoryReference, "prefers dark mode", "ref"); err != nil {
		t.Fatalf("upsert ref: %v", err)
	}
	all, _ := m.List(ctx, "u1")
	if len(all) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(all), all)
	}
}

func TestStructMemDetailDeleteSearch(t *testing.T) {
	m := NewStoreStructuralMemory(NewInMemoryStore(), fakeEmbedder{})
	ctx := context.Background()
	e, _ := m.Upsert(ctx, "u1", CategoryProject, "go module zzy wechat bot", "go 1.25 project")
	got, ok, _ := m.Detail(ctx, "u1", e.ID)
	if !ok || got.Detail != "go 1.25 project" {
		t.Fatalf("detail: %+v ok=%v", got, ok)
	}
	res, _ := m.Search(ctx, "u1", "wechat bot module", 5)
	if len(res) == 0 {
		t.Fatalf("expected search hit")
	}
	del, _ := m.Delete(ctx, "u1", e.ID)
	if !del {
		t.Fatalf("delete failed")
	}
	if _, ok, _ := m.Detail(ctx, "u1", e.ID); ok {
		t.Fatalf("entry still present after delete")
	}
}

func TestStructMemInjectPerCategory(t *testing.T) {
	m := NewStoreStructuralMemory(NewInMemoryStore(), fakeEmbedder{})
	ctx := context.Background()
	for _, cat := range memoryCategories {
		for i := 0; i < 3; i++ {
			if _, err := m.Upsert(ctx, "u1", cat, string(cat)+" point "+string(rune('a'+i)), "d"); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}
	}
	got, _ := m.Inject(ctx, "u1", "", 2)
	if len(got) != 8 {
		t.Fatalf("want 2 per 4 categories = 8, got %d", len(got))
	}
}

func TestStructMemUserIsolation(t *testing.T) {
	m := NewStoreStructuralMemory(NewInMemoryStore(), fakeEmbedder{})
	ctx := context.Background()
	m.Upsert(ctx, "u1", CategoryPersonal, "secret", "x")
	if all, _ := m.List(ctx, "u2"); len(all) != 0 {
		t.Fatalf("u2 should not see u1 memory")
	}
}
