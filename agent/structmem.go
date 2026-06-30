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
	store    Store
	embedder Embedder

	mu    sync.Mutex
	cache map[string]*structMemEntry
}

type structMemEntry struct {
	mu     sync.Mutex
	loaded bool
	memos  []storedMemo
}

// NewStoreStructuralMemory returns structural memory backed by store and
// embedder, mirroring how conversation history and grants persist (Redis in
// production, in-memory in dev).
func NewStoreStructuralMemory(store Store, embedder Embedder) StructuralMemory {
	return &storeStructuralMemory{store: store, embedder: embedder, cache: make(map[string]*structMemEntry)}
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

	// Embed before locking: it doesn't depend on stored state. We embed the
	// index (the summary) so merge keys on meaning, not detail wording.
	vecs, err := m.embedder.Embed(ctx, []string{index})
	if err != nil {
		return MemoEntry{}, fmt.Errorf("structmem: embed: %w", err)
	}
	vec := vector(vecs[0])
	now := time.Now().UTC()

	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)

	next := append([]storedMemo(nil), e.memos...)

	// Merge into the most-similar same-category entry above the threshold.
	best, bestScore := -1, float32(0)
	for i := range next {
		if next[i].Category != cat {
			continue
		}
		if s := cosine(vec, next[i].Vec); s > bestScore {
			best, bestScore = i, s
		}
	}
	var result MemoEntry
	if best >= 0 && bestScore >= structMergeThreshold {
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
	var q []float32
	if query = strings.TrimSpace(query); query != "" {
		vecs, err := m.embedder.Embed(ctx, []string{query})
		if err == nil {
			q = vecs[0]
		}
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)

	var out []MemoEntry
	for _, cat := range memoryCategories {
		group := make([]storedMemo, 0)
		for _, sm := range e.memos {
			if sm.Category == cat {
				group = append(group, sm)
			}
		}
		// Personal/project are stable context: keep them by recency. Feedback and
		// reference can be many, so keep only the ones relevant to the turn.
		byRelevance := q != nil && (cat == CategoryFeedback || cat == CategoryReference)
		sort.SliceStable(group, func(i, j int) bool {
			if byRelevance {
				si, sj := cosine(q, group[i].Vec), cosine(q, group[j].Vec)
				if si != sj {
					return si > sj
				}
			}
			return group[i].UpdatedAt.After(group[j].UpdatedAt)
		})
		for i := 0; i < len(group) && i < perCategory; i++ {
			out = append(out, group[i].MemoEntry)
		}
	}
	return out, nil
}

func (m *storeStructuralMemory) Search(ctx context.Context, userID, query string, limit int) ([]MemoEntry, error) {
	if userID == "" {
		return nil, errEmptyUserID
	}
	if limit <= 0 {
		limit = 5
	}
	var q []float32
	if query = strings.TrimSpace(query); query != "" {
		vecs, err := m.embedder.Embed(ctx, []string{query})
		if err != nil {
			return nil, fmt.Errorf("structmem: embed query: %w", err)
		}
		q = vecs[0]
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)

	memos := append([]storedMemo(nil), e.memos...)
	sort.SliceStable(memos, func(i, j int) bool {
		if q != nil {
			si, sj := cosine(q, memos[i].Vec), cosine(q, memos[j].Vec)
			if si != sj {
				return si > sj
			}
		}
		return memos[i].UpdatedAt.After(memos[j].UpdatedAt)
	})
	out := make([]MemoEntry, 0, limit)
	for i := 0; i < len(memos) && i < limit; i++ {
		out = append(out, memos[i].MemoEntry)
	}
	return out, nil
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
	texts := make([]string, 0, len(items))
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
		texts = append(texts, idx+"\n"+det)
	}
	var vecs [][]float32
	if len(texts) > 0 {
		v, err := m.embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("structmem: embed: %w", err)
		}
		vecs = v
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
			Vec:       vector(vecs[i]),
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
