package agent

import (
	"context"
	"testing"
)

// fakeSemantics is a controllable MemoSemantics for tests: it produces no
// vectors, dedups by exact Index match, and ranks by a fixed order. It records
// call counts so tests can assert the fallback path was taken.
type fakeSemantics struct {
	dedupCalls int
	rankCalls  int
	rankCands  []RankItem
	rankOrder  []int
}

func (f *fakeSemantics) Dedup(_ context.Context, note string, candidates []RankItem) (int, []float32, error) {
	f.dedupCalls++
	for i, c := range candidates {
		if c.Index == note {
			return i, nil, nil
		}
	}
	return -1, nil, nil
}

func (f *fakeSemantics) Rank(_ context.Context, _ string, candidates []RankItem, _ int) ([]int, error) {
	f.rankCalls++
	f.rankCands = candidates
	return f.rankOrder, nil
}

// TestEmbedSemanticsRankByCosine verifies the embedding-backed semantics order
// candidates by similarity of their stored vectors to the query.
func TestEmbedSemanticsRankByCosine(t *testing.T) {
	ctx := context.Background()
	s := NewEmbedSemantics(fakeEmbedder{})
	// fakeEmbedder is a bag-of-words hash, so a candidate sharing words with the
	// query scores higher.
	vecs, err := fakeEmbedder{}.Embed(ctx, []string{"alpha beta", "gamma delta"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	cands := []RankItem{{Index: "alpha beta", Vec: vecs[0]}, {Index: "gamma delta", Vec: vecs[1]}}
	order, err := s.Rank(ctx, "gamma delta", cands, 2)
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	if len(order) != 2 || order[0] != 1 {
		t.Fatalf("expected the matching candidate first, got order %v", order)
	}
}

// TestEmbedSemanticsUnavailable verifies Rank and Dedup signal unavailability
// (for a fallback) when embeddings are missing.
func TestEmbedSemanticsUnavailable(t *testing.T) {
	ctx := context.Background()
	s := NewEmbedSemantics(failingEmbedder{})
	if _, err := s.Rank(ctx, "q", []RankItem{{Index: "x"}}, 1); err == nil {
		t.Fatal("expected Rank to error when the query cannot be embedded")
	}
	if _, _, err := s.Dedup(ctx, "x", []RankItem{{Index: "y"}}); err == nil {
		t.Fatal("expected Dedup to error when the note cannot be embedded")
	}
}

// TestEmbedSemanticsDedupByCosine verifies cosine dedup merges an identical note
// (and returns a vector to persist) and keeps a different one.
func TestEmbedSemanticsDedupByCosine(t *testing.T) {
	ctx := context.Background()
	s := NewEmbedSemantics(fakeEmbedder{})
	vecs, _ := fakeEmbedder{}.Embed(ctx, []string{"prefers dark mode"})
	cands := []RankItem{{Index: "prefers dark mode", Vec: vecs[0]}}
	got, vec, err := s.Dedup(ctx, "prefers dark mode", cands)
	if err != nil || got != 0 {
		t.Fatalf("cosine dedup = (%d,%v), want (0,nil)", got, err)
	}
	if len(vec) == 0 {
		t.Fatal("expected Dedup to return a vector to persist")
	}
	if got, _, _ := s.Dedup(ctx, "lives in Tokyo", cands); got != -1 {
		t.Fatalf("unrelated note dedup = %d, want -1", got)
	}
}

// TestFallbackSemanticsFallsThrough verifies the composite falls back to the
// secondary for both Rank and Dedup when the primary is unavailable.
func TestFallbackSemanticsFallsThrough(t *testing.T) {
	ctx := context.Background()
	fs := &fakeSemantics{rankOrder: []int{0}}
	s := NewFallbackSemantics(NewEmbedSemantics(failingEmbedder{}), fs)

	order, err := s.Rank(ctx, "q", []RankItem{{Index: "a"}, {Index: "b"}}, 1)
	if err != nil || fs.rankCalls != 1 || len(order) != 1 || order[0] != 0 {
		t.Fatalf("rank fallback: order %v err %v calls %d", order, err, fs.rankCalls)
	}
	match, _, err := s.Dedup(ctx, "a", []RankItem{{Index: "a"}, {Index: "b"}})
	if err != nil || fs.dedupCalls != 1 || match != 0 {
		t.Fatalf("dedup fallback: match %d err %v calls %d", match, err, fs.dedupCalls)
	}
}

// TestLLMSemanticsRankGateSkipsSmall verifies the LLM Rank does no work (and makes
// no model call) when everything already fits within n.
func TestLLMSemanticsRankGateSkipsSmall(t *testing.T) {
	s := llmSemantics{client: nil} // client must not be used on the gated path
	order, err := s.Rank(context.Background(), "q", make([]RankItem, 3), 5)
	if err != nil || order != nil {
		t.Fatalf("expected the gate to skip ranking, got order %v err %v", order, err)
	}
}

// TestInjectUsesRankFallbackWhenEmbeddingsUnavailable verifies Inject routes the
// feedback/reference buckets through the Rank fallback when embeddings are
// unavailable, surfacing its pick first.
func TestInjectUsesRankFallbackWhenEmbeddingsUnavailable(t *testing.T) {
	ctx := context.Background()
	fs := &fakeSemantics{rankOrder: []int{2}}
	m := NewStoreStructuralMemory(NewInMemoryStore(),
		NewFallbackSemantics(NewEmbedSemantics(failingEmbedder{}), fs))

	for _, s := range []string{"alpha", "bravo", "charlie", "delta"} {
		mustUpsert(t, m, ctx, "alice", CategoryFeedback, s, s+" detail")
	}
	out, err := m.Inject(ctx, "alice", "which one?", 2)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if fs.rankCalls != 1 {
		t.Fatalf("expected the rank fallback to be called once, got %d", fs.rankCalls)
	}
	if len(out) != 2 {
		t.Fatalf("expected perCategory=2 entries, got %d", len(out))
	}
	if out[0].Index != fs.rankCands[2].Index {
		t.Fatalf("top entry = %q, want ranker pick %q", out[0].Index, fs.rankCands[2].Index)
	}
}
