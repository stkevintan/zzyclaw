package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryCategory is one of the four fixed buckets of dynamic structural memory.
// The set is closed: the reflector and tools only ever write these.
type MemoryCategory string

const (
	// CategoryPersonal: the user's character, preferences, role and style.
	CategoryPersonal MemoryCategory = "personal"
	// CategoryFeedback: explicit feedback, choices and corrections the user made.
	CategoryFeedback MemoryCategory = "feedback"
	// CategoryProject: facts about the current project the work concerns.
	CategoryProject MemoryCategory = "project"
	// CategoryReference: durable, referable facts that don't fit the others.
	CategoryReference MemoryCategory = "reference"
)

// memoryCategories is the canonical order used for prompt injection and listing.
var memoryCategories = []MemoryCategory{CategoryPersonal, CategoryFeedback, CategoryProject, CategoryReference}

// Valid reports whether c is one of the four known categories.
func (c MemoryCategory) Valid() bool {
	switch c {
	case CategoryPersonal, CategoryFeedback, CategoryProject, CategoryReference:
		return true
	}
	return false
}

// Label returns a human-readable heading for the category.
func (c MemoryCategory) Label() string {
	switch c {
	case CategoryPersonal:
		return "Personal"
	case CategoryFeedback:
		return "Feedback"
	case CategoryProject:
		return "Project"
	case CategoryReference:
		return "Reference"
	default:
		return string(c)
	}
}

const (
	// structIndexMaxRunes caps an injected index line so the system reminder stays
	// compact regardless of how the model phrases it.
	structIndexMaxRunes = 160
	// structMergeThreshold is the cosine similarity above which a new point is
	// merged into an existing same-category entry instead of stored separately.
	structMergeThreshold = 0.92
	// structHardCapPerCat is the absolute ceiling per category; the oldest
	// (least-recently-updated) entries are evicted past it.
	structHardCapPerCat = 50
	// structDefaultPerCat is the default number of indexes injected per category.
	structDefaultPerCat = 6
)

// MemoEntry is one durable structural-memory item. Only the Index is injected
// into the system reminder; the Detail is fetched on demand via recall.
type MemoEntry struct {
	ID        string         `json:"id"`
	Category  MemoryCategory `json:"category"`
	Index     string         `json:"index"`
	Detail    string         `json:"detail"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// MemoDraft is an unstored (index, detail) pair produced by reflection and
// consolidation.
type MemoDraft struct {
	Index  string
	Detail string
}

type storedMemo struct {
	MemoEntry
	Vec vector `json:"vec"`
}

// StructuralMemory is per-user, four-category long-term memory built up by idle
// reflection. Indexes are injected into the prompt; details are stored and
// recalled on demand. All operations are scoped to a single userID.
type StructuralMemory interface {
	// Upsert stores (or merges into a near-duplicate of) a categorized point and
	// returns the resulting entry.
	Upsert(ctx context.Context, userID string, cat MemoryCategory, index, detail string) (MemoEntry, error)
	// Inject returns up to perCategory entries per category for the prompt:
	// personal/project ranked by recency, feedback/reference by relevance to query.
	Inject(ctx context.Context, userID, query string, perCategory int) ([]MemoEntry, error)
	// Search ranks all of a user's entries by relevance to query (recall tool).
	Search(ctx context.Context, userID, query string, limit int) ([]MemoEntry, error)
	// List returns every entry, ordered by category then recency.
	List(ctx context.Context, userID string) ([]MemoEntry, error)
	// Detail returns a single entry by id.
	Detail(ctx context.Context, userID, id string) (MemoEntry, bool, error)
	// Delete removes an entry by id.
	Delete(ctx context.Context, userID, id string) (bool, error)
	// ReplaceCategory swaps every entry in cat for the consolidated drafts.
	ReplaceCategory(ctx context.Context, userID string, cat MemoryCategory, items []MemoDraft) error
}

type storeStructuralMemory struct {
	store Store
	sem   MemoSemantics

	mu    sync.Mutex
	cache map[string]*structMemEntry
}

type structMemEntry struct {
	mu     sync.Mutex
	loaded bool
	memos  []storedMemo
}

// NewStoreStructuralMemory returns structural memory backed by store and sem,
// mirroring how conversation history and grants persist (Redis in production,
// in-memory in dev). All embedding/LLM behavior lives behind sem, so the store
// itself never depends on a concrete embedder.
func NewStoreStructuralMemory(store Store, sem MemoSemantics) StructuralMemory {
	return &storeStructuralMemory{store: store, sem: sem, cache: make(map[string]*structMemEntry)}
}

func structMemKey(userID string) string { return "structmem:" + userID }

type structMemRecord struct {
	Memos []storedMemo `json:"memos"`
}

func (m *storeStructuralMemory) entry(userID string) *structMemEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.cache[userID]
	if !ok {
		e = &structMemEntry{}
		m.cache[userID] = e
	}
	return e
}

// ensureLoaded hydrates e once; a transient error leaves it unloaded so a later
// call retries rather than caching an empty list. Callers must hold e.mu.
func (m *storeStructuralMemory) ensureLoaded(ctx context.Context, userID string, e *structMemEntry) {
	if e.loaded {
		return
	}
	data, err := m.store.GetMeta(ctx, structMemKey(userID))
	if err != nil {
		return
	}
	if len(data) > 0 {
		var rec structMemRecord
		if json.Unmarshal(data, &rec) == nil {
			e.memos = rec.Memos
		}
	}
	e.loaded = true
}

func (m *storeStructuralMemory) persist(ctx context.Context, userID string, memos []storedMemo) error {
	data, err := json.Marshal(structMemRecord{Memos: memos})
	if err != nil {
		return err
	}
	return m.store.SetMeta(ctx, structMemKey(userID), data)
}

func (m *storeStructuralMemory) Upsert(ctx context.Context, userID string, cat MemoryCategory, index, detail string) (MemoEntry, error) {
	if userID == "" {
		return MemoEntry{}, errEmptyUserID
	}
	if !cat.Valid() {
		return MemoEntry{}, fmt.Errorf("structmem: invalid category %q", cat)
	}
	index = clipRunes(strings.TrimSpace(index), structIndexMaxRunes)
	detail = clip(detail)
	if index == "" {
		return MemoEntry{}, errBlankMemory
	}
	if detail == "" {
		detail = index
	}

	// Phase 1: snapshot the same-category candidates (with their ids) under the
	// lock, then release it — Dedup may embed or call the model, and the per-user
	// lock must never be held across that I/O.
	e := m.entry(userID)
	e.mu.Lock()
	m.ensureLoaded(ctx, userID, e)
	var candID []string
	cands := make([]RankItem, 0)
	for _, sm := range e.memos {
		if sm.Category == cat {
			candID = append(candID, sm.ID)
			cands = append(cands, RankItem{Index: sm.Index, Vec: sm.Vec})
		}
	}
	e.mu.Unlock()

	// Phase 2: decide the merge target and get the vector to persist. Dedup
	// encapsulates the strategy (embedding cosine or a small LLM), so Upsert never
	// embeds or compares vectors itself.
	match, rawVec, _ := m.sem.Dedup(ctx, index, cands)
	vec := vector(rawVec)
	var mergeID string
	if match >= 0 && match < len(candID) {
		mergeID = candID[match]
	}

	// Phase 3: apply under the lock, resolving the merge target by id so a
	// concurrent write between the phases can't corrupt the result.
	now := time.Now().UTC()
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)
	next := append([]storedMemo(nil), e.memos...)
	best := -1
	if mergeID != "" {
		for i := range next {
			if next[i].ID == mergeID {
				best = i
				break
			}
		}
	}
	var result MemoEntry
	if best >= 0 {
		next[best].Index = index
		next[best].Detail = detail
		next[best].UpdatedAt = now
		next[best].Vec = vec
		result = next[best].MemoEntry
	} else {
		sm := storedMemo{MemoEntry: MemoEntry{ID: newID(), Category: cat, Index: index, Detail: detail, CreatedAt: now, UpdatedAt: now}, Vec: vec}
		next = append(next, sm)
		result = sm.MemoEntry
	}
	next = evictCategory(next, cat, structHardCapPerCat)
	if err := m.persist(ctx, userID, next); err != nil {
		return MemoEntry{}, err
	}
	e.memos = next
	return result, nil
}

func (m *storeStructuralMemory) Inject(ctx context.Context, userID, query string, perCategory int) ([]MemoEntry, error) {
	if userID == "" {
		return nil, errEmptyUserID
	}
	if perCategory <= 0 {
		perCategory = structDefaultPerCat
	}
	// Snapshot each category (recency-ordered) under the lock, then release it
	// before any ranker call so the user's memory lock is never held across a
	// network request (embedding or LLM).
	e := m.entry(userID)
	e.mu.Lock()
	m.ensureLoaded(ctx, userID, e)
	groups := make(map[MemoryCategory][]storedMemo, len(memoryCategories))
	for _, sm := range e.memos {
		groups[sm.Category] = append(groups[sm.Category], sm)
	}
	e.mu.Unlock()
	for cat := range groups {
		sortByRecency(groups[cat])
	}

	// Feedback and reference can be large, so rank them by relevance to the turn
	// and keep the top few; personal and project are stable context kept by
	// recency. Rank both relevance buckets in one call so the query is embedded
	// once, then fill each category's quota from the shared ranking.
	rel := append(append([]storedMemo(nil), groups[CategoryFeedback]...), groups[CategoryReference]...)
	ranked := m.rankItems(ctx, query, rel, 2*perCategory)
	picks := make(map[MemoryCategory][]MemoEntry, len(memoryCategories))
	for _, sm := range ranked {
		if len(picks[sm.Category]) < perCategory {
			picks[sm.Category] = append(picks[sm.Category], sm.MemoEntry)
		}
	}
	for _, cat := range []MemoryCategory{CategoryPersonal, CategoryProject} {
		picks[cat] = topEntries(groups[cat], perCategory)
	}

	var out []MemoEntry
	for _, cat := range memoryCategories {
		out = append(out, picks[cat]...)
	}
	return out, nil
}

// rankItems reorders group best-first by relevance to query using the configured
// ranker, keeping recency for entries the ranker omits so nothing is ever
// dropped. It returns group unchanged when there is no ranker, no query, or the
// ranker declines. group is assumed to already be in recency order.
func (m *storeStructuralMemory) rankItems(ctx context.Context, query string, group []storedMemo, keep int) []storedMemo {
	if m.sem == nil || strings.TrimSpace(query) == "" || len(group) == 0 {
		return group
	}
	cands := make([]RankItem, len(group))
	for i, sm := range group {
		cands[i] = RankItem{Index: sm.Index, Vec: sm.Vec}
	}
	order, err := m.sem.Rank(ctx, query, cands, keep)
	if err != nil || len(order) == 0 {
		return group
	}
	out := make([]storedMemo, 0, len(group))
	seen := make(map[int]bool, len(group))
	for _, idx := range order {
		if idx >= 0 && idx < len(group) && !seen[idx] {
			seen[idx] = true
			out = append(out, group[idx])
		}
	}
	for i, sm := range group {
		if !seen[i] {
			out = append(out, sm)
		}
	}
	return out
}

// sortByRecency orders memos most-recently-updated first, in place.
func sortByRecency(memos []storedMemo) {
	sort.SliceStable(memos, func(i, j int) bool {
		return memos[i].UpdatedAt.After(memos[j].UpdatedAt)
	})
}

// topEntries returns up to n entries from a slice as MemoEntry values.
func topEntries(memos []storedMemo, n int) []MemoEntry {
	out := make([]MemoEntry, 0, n)
	for i := 0; i < len(memos) && i < n; i++ {
		out = append(out, memos[i].MemoEntry)
	}
	return out
}

func (m *storeStructuralMemory) Search(ctx context.Context, userID, query string, limit int) ([]MemoEntry, error) {
	if userID == "" {
		return nil, errEmptyUserID
	}
	if limit <= 0 {
		limit = 5
	}
	// Snapshot under the lock, then release before ranking.
	e := m.entry(userID)
	e.mu.Lock()
	m.ensureLoaded(ctx, userID, e)
	memos := append([]storedMemo(nil), e.memos...)
	e.mu.Unlock()

	sortByRecency(memos)
	memos = m.rankItems(ctx, query, memos, limit)
	return topEntries(memos, limit), nil
}

func (m *storeStructuralMemory) List(ctx context.Context, userID string) ([]MemoEntry, error) {
	if userID == "" {
		return nil, errEmptyUserID
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)
	out := make([]MemoEntry, 0, len(e.memos))
	for _, sm := range e.memos {
		out = append(out, sm.MemoEntry)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return catRank(out[i].Category) < catRank(out[j].Category)
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (m *storeStructuralMemory) Detail(ctx context.Context, userID, id string) (MemoEntry, bool, error) {
	if userID == "" || id == "" {
		return MemoEntry{}, false, nil
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)
	for _, sm := range e.memos {
		if sm.ID == id {
			return sm.MemoEntry, true, nil
		}
	}
	return MemoEntry{}, false, nil
}

func (m *storeStructuralMemory) Delete(ctx context.Context, userID, id string) (bool, error) {
	if userID == "" || id == "" {
		return false, nil
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)
	idx := -1
	for i, sm := range e.memos {
		if sm.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	next := make([]storedMemo, 0, len(e.memos)-1)
	next = append(next, e.memos[:idx]...)
	next = append(next, e.memos[idx+1:]...)
	if err := m.persist(ctx, userID, next); err != nil {
		return false, err
	}
	e.memos = next
	return true, nil
}

func (m *storeStructuralMemory) ReplaceCategory(ctx context.Context, userID string, cat MemoryCategory, items []MemoDraft) error {
	if userID == "" {
		return errEmptyUserID
	}
	if !cat.Valid() {
		return fmt.Errorf("structmem: invalid category %q", cat)
	}
	clean := make([]MemoDraft, 0, len(items))
	for _, it := range items {
		idx := clipRunes(strings.TrimSpace(it.Index), structIndexMaxRunes)
		if idx == "" {
			continue
		}
		det := clip(it.Detail)
		if det == "" {
			det = idx
		}
		clean = append(clean, MemoDraft{Index: idx, Detail: det})
	}
	// Embed each draft's index (via Dedup with no candidates) before locking, so
	// recall can rank consolidated entries. Nil vectors when embeddings are
	// unavailable — Dedup degrades rather than failing.
	vecs := make([]vector, len(clean))
	for i, d := range clean {
		if _, v, _ := m.sem.Dedup(ctx, d.Index, nil); len(v) > 0 {
			vecs[i] = vector(v)
		}
	}
	now := time.Now().UTC()
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)
	next := make([]storedMemo, 0, len(e.memos))
	for _, sm := range e.memos {
		if sm.Category != cat {
			next = append(next, sm)
		}
	}
	for i, d := range clean {
		next = append(next, storedMemo{
			MemoEntry: MemoEntry{ID: newID(), Category: cat, Index: d.Index, Detail: d.Detail, CreatedAt: now, UpdatedAt: now},
			Vec:       vecs[i],
		})
	}
	if err := m.persist(ctx, userID, next); err != nil {
		return err
	}
	e.memos = next
	return nil
}

// evictCategory drops the least-recently-updated entries of cat until at most
// cap remain, leaving other categories untouched.
func evictCategory(memos []storedMemo, cat MemoryCategory, cap int) []storedMemo {
	idxs := make([]int, 0)
	for i := range memos {
		if memos[i].Category == cat {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) <= cap {
		return memos
	}
	sort.SliceStable(idxs, func(a, b int) bool {
		return memos[idxs[a]].UpdatedAt.Before(memos[idxs[b]].UpdatedAt)
	})
	drop := make(map[int]struct{})
	for i := 0; i < len(idxs)-cap; i++ {
		drop[idxs[i]] = struct{}{}
	}
	next := make([]storedMemo, 0, len(memos)-len(drop))
	for i := range memos {
		if _, ok := drop[i]; ok {
			continue
		}
		next = append(next, memos[i])
	}
	return next
}

func catRank(c MemoryCategory) int {
	for i, mc := range memoryCategories {
		if mc == c {
			return i
		}
	}
	return len(memoryCategories)
}

func clipRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return strings.TrimSpace(string(r[:max]))
	}
	return s
}

var errBlankMemory = &memoryError{"memory text must not be empty"}
var errEmptyUserID = &memoryError{"user ID must not be empty"}

type memoryError struct{ msg string }

func (e *memoryError) Error() string { return e.msg }
