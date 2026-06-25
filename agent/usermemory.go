package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxFactsPerUser bounds a user's long-term memory so it can never grow without
// limit. When the cap is exceeded the oldest facts are dropped first.
const maxFactsPerUser = 500

// Fact is a single durable note remembered for a user. Unlike conversation
// history (which is per-session and compacted away), a Fact persists across
// sessions and is selectively surfaced back into the system prompt.
type Fact struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// UserMemory is a long-term, per-user memory layer kept separate from
// conversation history. It stores durable facts the agent (or user) chooses to
// remember and retrieves the most relevant ones on demand. Every operation is
// scoped to a single userID: one user's memory is never visible to another.
//
// This first implementation ranks by keyword overlap and recency (no
// embeddings), which keeps it dependency-free; the interface is deliberately
// small so a vector- or service-backed implementation (e.g. mem0) can be
// substituted later without touching the engine.
type UserMemory interface {
	// Add stores text as a new fact for userID and returns it. Blank text is
	// rejected; an exact (case-insensitive) duplicate returns the existing fact
	// instead of creating another.
	Add(ctx context.Context, userID, text string) (Fact, error)
	// Search returns up to limit facts for userID ranked by relevance to query
	// (keyword overlap, then recency). An empty query returns the most recent
	// facts. limit <= 0 uses a small default.
	Search(ctx context.Context, userID, query string, limit int) ([]Fact, error)
	// List returns all of userID's facts, most recent first.
	List(ctx context.Context, userID string) ([]Fact, error)
	// Delete removes the fact with id from userID's memory. found reports whether
	// a matching fact existed.
	Delete(ctx context.Context, userID, id string) (found bool, err error)
}

// storeUserMemory persists each user's facts through the shared Store (Redis in
// production, in-memory in dev), mirroring how per-user grants and session
// indexes are stored. Each user's facts are cached in memory after first access;
// store I/O happens under that user's own lock so one user's latency never
// blocks another.
type storeUserMemory struct {
	store Store

	mu    sync.Mutex
	cache map[string]*userMemEntry
}

// userMemEntry is one user's cached fact list, hydrated from the store at most
// once on success.
type userMemEntry struct {
	mu     sync.Mutex
	loaded bool
	facts  []Fact
}

// NewStoreUserMemory returns a per-user memory backed by store. It shares the
// same durability as conversation memory: facts survive restarts when the store
// is Redis-backed, and are process-local otherwise.
func NewStoreUserMemory(store Store) UserMemory {
	return &storeUserMemory{store: store, cache: make(map[string]*userMemEntry)}
}

func memoryMetaKey(userID string) string { return "memory:" + userID }

type memoryRecord struct {
	Facts []Fact `json:"facts"`
}

// entry returns the per-user cache entry, creating it on first access. Only the
// global cache-map lookup is guarded here; per-user store I/O happens under the
// entry's own lock.
func (m *storeUserMemory) entry(userID string) *userMemEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.cache[userID]
	if !ok {
		e = &userMemEntry{}
		m.cache[userID] = e
	}
	return e
}

// ensureLoaded hydrates e from the store once. A transient store error leaves
// e.loaded false so a later call retries rather than caching an empty list.
// Callers must hold e.mu.
func (m *storeUserMemory) ensureLoaded(ctx context.Context, userID string, e *userMemEntry) {
	if e.loaded {
		return
	}
	data, err := m.store.GetMeta(ctx, memoryMetaKey(userID))
	if err != nil {
		return
	}
	if len(data) > 0 {
		var rec memoryRecord
		if json.Unmarshal(data, &rec) == nil {
			e.facts = rec.Facts
		}
	}
	e.loaded = true
}

// persist writes e's facts back to the store. Callers must hold e.mu.
func (m *storeUserMemory) persist(ctx context.Context, userID string, e *userMemEntry) error {
	data, err := json.Marshal(memoryRecord{Facts: e.facts})
	if err != nil {
		return err
	}
	return m.store.SetMeta(ctx, memoryMetaKey(userID), data)
}

func (m *storeUserMemory) Add(ctx context.Context, userID, text string) (Fact, error) {
	text = strings.TrimSpace(text)
	if userID == "" || text == "" {
		return Fact{}, errBlankMemory
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)

	// Collapse exact (case-insensitive) duplicates so repeated remembers don't
	// bloat the store or skew the grant-free recency ranking.
	for _, f := range e.facts {
		if strings.EqualFold(f.Text, text) {
			return f, nil
		}
	}

	f := Fact{ID: newFactID(), Text: text, CreatedAt: time.Now().UTC()}
	e.facts = append(e.facts, f)
	// Enforce the per-user cap by dropping the oldest facts.
	if len(e.facts) > maxFactsPerUser {
		e.facts = e.facts[len(e.facts)-maxFactsPerUser:]
	}
	if err := m.persist(ctx, userID, e); err != nil {
		return Fact{}, err
	}
	return f, nil
}

func (m *storeUserMemory) Search(ctx context.Context, userID, query string, limit int) ([]Fact, error) {
	if limit <= 0 {
		limit = 5
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)

	facts := append([]Fact(nil), e.facts...)
	terms := tokenize(query)
	if len(terms) == 0 {
		// No query: most recent first.
		sort.SliceStable(facts, func(i, j int) bool {
			return facts[i].CreatedAt.After(facts[j].CreatedAt)
		})
		return capFacts(facts, limit), nil
	}

	type scored struct {
		f     Fact
		score int
	}
	ranked := make([]scored, 0, len(facts))
	for _, f := range facts {
		if s := overlap(terms, tokenize(f.Text)); s > 0 {
			ranked = append(ranked, scored{f: f, score: s})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].f.CreatedAt.After(ranked[j].f.CreatedAt)
	})
	out := make([]Fact, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.f)
	}
	return capFacts(out, limit), nil
}

func (m *storeUserMemory) List(ctx context.Context, userID string) ([]Fact, error) {
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)
	facts := append([]Fact(nil), e.facts...)
	sort.SliceStable(facts, func(i, j int) bool {
		return facts[i].CreatedAt.After(facts[j].CreatedAt)
	})
	return facts, nil
}

func (m *storeUserMemory) Delete(ctx context.Context, userID, id string) (bool, error) {
	if userID == "" || id == "" {
		return false, nil
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)

	idx := -1
	for i, f := range e.facts {
		if f.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	e.facts = append(e.facts[:idx], e.facts[idx+1:]...)
	if err := m.persist(ctx, userID, e); err != nil {
		return false, err
	}
	return true, nil
}

// errBlankMemory is returned when an empty fact is added.
var errBlankMemory = &memoryError{"memory text must not be empty"}

type memoryError struct{ msg string }

func (e *memoryError) Error() string { return e.msg }

// newFactID returns a short random hex identifier for a fact. Randomness (rather
// than a counter) avoids cross-process collisions when the store is shared.
func newFactID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read essentially never fails; fall back to a timestamp.
		return strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	}
	return hex.EncodeToString(b[:])
}

// tokenize lowercases s and splits it into a set of word tokens (letters and
// digits), dropping very short tokens that add noise to keyword matching.
func tokenize(s string) map[string]struct{} {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !isWordRune(r)
	})
	set := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if len(f) >= 2 {
			set[f] = struct{}{}
		}
	}
	return set
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
		r >= 0x80 // keep multibyte (e.g. CJK) runes as word characters
}

// overlap counts how many query terms appear in the fact's token set.
func overlap(query, fact map[string]struct{}) int {
	n := 0
	for t := range query {
		if _, ok := fact[t]; ok {
			n++
		}
	}
	return n
}

func capFacts(facts []Fact, limit int) []Fact {
	if limit > 0 && len(facts) > limit {
		return facts[:limit]
	}
	return facts
}
