package agent

import (
	"context"
	"errors"
	"testing"
)

// failingEmbedder always errors, standing in for an unavailable or unconfigured
// embeddings backend.
type failingEmbedder struct{}

func (failingEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	return nil, errors.New("embeddings unavailable")
}

// TestStructuralMemoryWorksWithoutEmbeddings guards that remember/recall keep
// functioning when the embeddings backend is unavailable: entries are stored and
// returned (by recency), just without semantic ranking or merging.
func TestStructuralMemoryWorksWithoutEmbeddings(t *testing.T) {
	ctx := context.Background()
	m := NewStoreStructuralMemory(NewInMemoryStore(), NewEmbedSemantics(failingEmbedder{}))

	if _, err := m.Upsert(ctx, "alice", CategoryProject, "uses Go 1.25", "the project targets Go 1.25"); err != nil {
		t.Fatalf("upsert must succeed without embeddings: %v", err)
	}
	if _, err := m.Upsert(ctx, "alice", CategoryProject, "uses SQLite", "storage is SQLite"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := m.Inject(ctx, "alice", "what database?", 5)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both entries returned by recency, got %d", len(got))
	}
}

// TestUpsertLLMDedupFallbackWithoutEmbeddings guards that re-remembering the same
// note still merges (no duplicate) when embeddings are unavailable, via the LLM
// dedup fallback, while a genuinely different note is kept.
func TestUpsertLLMDedupFallbackWithoutEmbeddings(t *testing.T) {
	ctx := context.Background()
	// fakeSemantics dedups by exact Index match, standing in for the small LLM.
	m := NewStoreStructuralMemory(NewInMemoryStore(),
		NewFallbackSemantics(NewEmbedSemantics(failingEmbedder{}), &fakeSemantics{}))

	mustUpsert(t, m, ctx, "alice", CategoryPersonal, "prefers dark mode", "uses a dark theme")
	// Same note remembered again (different detail): should merge, not duplicate.
	mustUpsert(t, m, ctx, "alice", CategoryPersonal, "prefers dark mode", "always dark")
	// Unrelated note: kept separately.
	mustUpsert(t, m, ctx, "alice", CategoryPersonal, "lives in Tokyo", "timezone JST")

	all, _ := m.List(ctx, "alice")
	if len(all) != 2 {
		t.Fatalf("want 2 entries after LLM dedup fallback, got %d: %+v", len(all), all)
	}
}
