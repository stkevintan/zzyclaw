package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	"zzy/copilot"
)

// errSemanticsUnavailable signals that a strategy cannot decide right now (e.g.
// dedup/rank without a usable embedding), so a composite should try a fallback.
var errSemanticsUnavailable = errors.New("semantics: unavailable")

// embedSemantics implements MemoSemantics with embeddings: cosine similarity for
// both duplicate detection and ranking. When embeddings are unavailable it
// reports errSemanticsUnavailable from Dedup and Rank so a fallback (e.g. a small
// LLM) can take over.
type embedSemantics struct {
	embedder Embedder
	warned   atomic.Bool
}

// NewEmbedSemantics returns embedding-backed MemoSemantics.
func NewEmbedSemantics(embedder Embedder) MemoSemantics {
	return &embedSemantics{embedder: embedder}
}

func (s *embedSemantics) Dedup(ctx context.Context, note string, candidates []RankItem) (int, []float32, error) {
	vecs, err := s.embedder.Embed(ctx, []string{note})
	if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		s.warn(err)
		// No vector to compare or persist: let a fallback decide.
		return -1, nil, errSemanticsUnavailable
	}
	noteVec := vecs[0]
	best, bestScore := -1, float32(0)
	for i, c := range candidates {
		if sc := cosine(noteVec, c.Vec); sc > bestScore {
			best, bestScore = i, sc
		}
	}
	if best >= 0 && bestScore >= structMergeThreshold {
		return best, noteVec, nil
	}
	return -1, noteVec, nil
}

func (s *embedSemantics) Rank(ctx context.Context, query string, candidates []RankItem, _ int) ([]int, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, errSemanticsUnavailable
	}
	q := vecs[0]
	order := make([]int, len(candidates))
	for i := range order {
		order[i] = i
	}
	// Stable sort so equal scores (including all-zero when a candidate lacks a
	// vector) preserve the caller's incoming order, which is recency.
	sort.SliceStable(order, func(a, b int) bool {
		return cosine(q, candidates[order[a]].Vec) > cosine(q, candidates[order[b]].Vec)
	})
	return order, nil
}

// warn logs, once, that the embeddings backend is unavailable.
func (s *embedSemantics) warn(err error) {
	if !s.warned.Swap(true) {
		slog.Warn("embeddings unavailable; structural memory falling back to LLM dedup/ranking", "err", err)
	}
}

// llmSemantics implements MemoSemantics with a chat model: it dedups and ranks by
// asking the model. It produces no vectors (Dedup returns a nil vector). Used as
// the fallback for embedSemantics when embeddings are unavailable, so it runs
// only in that degraded mode; prefer a small, cheap model. To bound cost, Rank
// does nothing unless there are more candidates than the caller keeps.
type llmSemantics struct {
	client *copilot.Client
}

// NewLLMSemantics returns MemoSemantics backed by the chat model, used as the
// dedup/rank fallback when embeddings are unavailable.
func NewLLMSemantics(client *copilot.Client) MemoSemantics {
	return llmSemantics{client: client}
}

const dedupSystemPrompt = `You decide whether a new memory note duplicates one of the existing notes.
Given the new note and a numbered list of existing notes, if the new note states the same fact as
one of them (even if worded differently), respond {"duplicate": <number>} with that note's number.
If it is genuinely new, respond {"duplicate": -1}. Use only the numbers shown.`

type dedupResult struct {
	Duplicate int `json:"duplicate"`
}

func (s llmSemantics) Dedup(ctx context.Context, note string, candidates []RankItem) (int, []float32, error) {
	if len(candidates) == 0 {
		return -1, nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "New note: %s\n\nExisting notes:\n", note)
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d. %s\n", i, c.Index)
	}
	out, err := copilot.Parse[dedupResult](ctx, s.client, dedupSystemPrompt, b.String())
	if err != nil {
		return -1, nil, err
	}
	if out == nil || out.Duplicate < 0 || out.Duplicate >= len(candidates) {
		return -1, nil, nil
	}
	return out.Duplicate, nil, nil
}

const rankSystemPrompt = `You rank memory notes by how relevant each is to a query.
You are given a query and a numbered list of notes. Return the numbers of the most
relevant notes, most relevant first, as JSON {"order":[...]} using exactly the numbers
shown. Include only clearly relevant notes and omit the rest; never invent numbers.`

type rankResult struct {
	Order []int `json:"order"`
}

func (s llmSemantics) Rank(ctx context.Context, query string, candidates []RankItem, n int) ([]int, error) {
	if len(candidates) <= n {
		// Everything fits; ranking would only reorder, not prune. Skip the call.
		return nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Query: %s\n\nNotes:\n", query)
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d. %s\n", i, c.Index)
	}
	fmt.Fprintf(&b, "\nReturn up to %d note numbers, most relevant first.", n)
	out, err := copilot.Parse[rankResult](ctx, s.client, rankSystemPrompt, b.String())
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	return out.Order, nil
}

// fallbackSemantics chains strategies: it tries each in order and moves to the
// next when one reports it cannot decide (a non-nil error). This pairs, e.g.,
// embeddings with a small LLM so dedup and ranking keep working when embeddings
// are down; more strategies can be chained if needed.
type fallbackSemantics struct {
	strategies []MemoSemantics
}

// NewFallbackSemantics composes strategies into a fallback chain, tried in order.
// The first that succeeds wins; a strategy that returns an error is skipped for
// the next one.
func NewFallbackSemantics(strategies ...MemoSemantics) MemoSemantics {
	return fallbackSemantics{strategies: strategies}
}

func (s fallbackSemantics) Dedup(ctx context.Context, note string, candidates []RankItem) (int, []float32, error) {
	var lastErr error = errors.New("no strategies configured")
	for _, st := range s.strategies {
		match, vec, err := st.Dedup(ctx, note, candidates)
		if err == nil {
			return match, vec, nil
		}
		lastErr = err
	}
	return -1, nil, lastErr
}

func (s fallbackSemantics) Rank(ctx context.Context, query string, candidates []RankItem, n int) ([]int, error) {
	var lastErr error = errors.New("no strategies configured")
	for _, st := range s.strategies {
		order, err := st.Rank(ctx, query, candidates, n)
		if err == nil {
			return order, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
