package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
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

// storedFact is a Fact plus its embedding vector. The vector is kept internal to
// the memory layer (never returned to tools or the engine) and is what powers
// semantic search.
type storedFact struct {
	Fact
	Vec vector `json:"vec"`
}

// Embedder turns text into vectors for semantic similarity search. One vector is
// returned per input string, in input order. It is the only external dependency
// of the memory layer; in production it is backed by the Copilot embeddings API.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// UserMemory is a long-term, per-user memory layer kept separate from
// conversation history. It stores durable facts the agent (or user) chooses to
// remember and retrieves the most relevant ones on demand. Every operation is
// scoped to a single userID: one user's memory is never visible to another.
//
// Relevance is ranked by semantic similarity: each fact is embedded once when
// stored, and a query is embedded and compared by cosine similarity. The
// interface is deliberately small so a service-backed implementation (e.g. mem0)
// can be substituted later without touching the engine.
type UserMemory interface {
	// Add stores text as a new fact for userID and returns it. Blank text is
	// rejected; an exact (case-insensitive) duplicate returns the existing fact
	// instead of creating another.
	Add(ctx context.Context, userID, text string) (Fact, error)
	// Search returns up to limit facts for userID ranked by semantic relevance to
	// query. An empty query returns the most recent facts. limit <= 0 uses a
	// small default.
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
	store    Store
	embedder Embedder

	mu    sync.Mutex
	cache map[string]*userMemEntry
}

// userMemEntry is one user's cached fact list, hydrated from the store at most
// once on success.
type userMemEntry struct {
	mu     sync.Mutex
	loaded bool
	facts  []storedFact
}

// NewStoreUserMemory returns a per-user memory backed by store and embedder. It
// shares the same durability as conversation memory: facts survive restarts when
// the store is Redis-backed, and are process-local otherwise. embedder must be
// non-nil; every fact is embedded when stored so search can rank semantically.
func NewStoreUserMemory(store Store, embedder Embedder) UserMemory {
	return &storeUserMemory{store: store, embedder: embedder, cache: make(map[string]*userMemEntry)}
}

func memoryMetaKey(userID string) string { return "memory:" + userID }

type memoryRecord struct {
	Facts []storedFact `json:"facts"`
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

// persist writes facts back to the store. It takes the slice explicitly (rather
// than the entry) so callers can persist the new state *before* swapping it into
// the cache, keeping the cache and store from diverging if a write fails.
func (m *storeUserMemory) persist(ctx context.Context, userID string, facts []storedFact) error {
	data, err := json.Marshal(memoryRecord{Facts: facts})
	if err != nil {
		return err
	}
	return m.store.SetMeta(ctx, memoryMetaKey(userID), data)
}

func (m *storeUserMemory) Add(ctx context.Context, userID, text string) (Fact, error) {
	text = strings.TrimSpace(text)
	if userID == "" {
		return Fact{}, errEmptyUserID
	}
	if text == "" {
		return Fact{}, errBlankMemory
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)

	// Collapse exact (case-insensitive) duplicates so repeated remembers don't
	// bloat the store or skew ranking, and to avoid a needless embedding call.
	for _, f := range e.facts {
		if strings.EqualFold(f.Text, text) {
			return f.Fact, nil
		}
	}

	// Embed once at write time; failure fails the remember so we never store a
	// fact that semantic search can't see.
	vecs, err := m.embedder.Embed(ctx, []string{text})
	if err != nil {
		return Fact{}, fmt.Errorf("memory: embed fact: %w", err)
	}

	f := storedFact{
		Fact: Fact{ID: newFactID(), Text: text, CreatedAt: time.Now().UTC()},
		Vec:  vector(vecs[0]),
	}
	// Build the next slice, persist it, and only then swap it into the cache, so
	// a write failure leaves the cache untouched. Copying into a freshly sized
	// slice when over the cap also lets the dropped oldest facts be GC'd rather
	// than pinned by the cached backing array.
	newFacts := make([]storedFact, 0, len(e.facts)+1)
	newFacts = append(newFacts, e.facts...)
	newFacts = append(newFacts, f)
	if len(newFacts) > maxFactsPerUser {
		active := make([]storedFact, maxFactsPerUser)
		copy(active, newFacts[len(newFacts)-maxFactsPerUser:])
		newFacts = active
	}
	if err := m.persist(ctx, userID, newFacts); err != nil {
		return Fact{}, err
	}
	e.facts = newFacts
	return f.Fact, nil
}

func (m *storeUserMemory) Search(ctx context.Context, userID, query string, limit int) ([]Fact, error) {
	if limit <= 0 {
		limit = 5
	}
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)

	facts := append([]storedFact(nil), e.facts...)
	if strings.TrimSpace(query) == "" || len(facts) == 0 {
		// No query: most recent first.
		sort.SliceStable(facts, func(i, j int) bool {
			return facts[i].CreatedAt.After(facts[j].CreatedAt)
		})
		return capFacts(toFacts(facts), limit), nil
	}

	vecs, err := m.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("memory: embed query: %w", err)
	}
	q := vecs[0]

	type scored struct {
		f     storedFact
		score float32
	}
	ranked := make([]scored, 0, len(facts))
	for _, f := range facts {
		if len(f.Vec) == 0 {
			continue
		}
		ranked = append(ranked, scored{f: f, score: cosine(q, f.Vec)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].f.CreatedAt.After(ranked[j].f.CreatedAt)
	})
	out := make([]storedFact, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.f)
	}
	return capFacts(toFacts(out), limit), nil
}

func (m *storeUserMemory) List(ctx context.Context, userID string) ([]Fact, error) {
	e := m.entry(userID)
	e.mu.Lock()
	defer e.mu.Unlock()
	m.ensureLoaded(ctx, userID, e)
	facts := append([]storedFact(nil), e.facts...)
	sort.SliceStable(facts, func(i, j int) bool {
		return facts[i].CreatedAt.After(facts[j].CreatedAt)
	})
	return toFacts(facts), nil
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
	// Build the remaining set, persist it, then swap into the cache only on
	// success (same cache/store-consistency reasoning as Add).
	newFacts := make([]storedFact, 0, len(e.facts)-1)
	newFacts = append(newFacts, e.facts[:idx]...)
	newFacts = append(newFacts, e.facts[idx+1:]...)
	if err := m.persist(ctx, userID, newFacts); err != nil {
		return false, err
	}
	e.facts = newFacts
	return true, nil
}

// errBlankMemory is returned when an empty fact is added.
var errBlankMemory = &memoryError{"memory text must not be empty"}

// errEmptyUserID is returned when an operation is missing a user scope.
var errEmptyUserID = &memoryError{"user ID must not be empty"}

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

// toFacts strips the embedding vectors, returning the public Fact view callers
// (tools, engine) see.
func toFacts(stored []storedFact) []Fact {
	out := make([]Fact, len(stored))
	for i, s := range stored {
		out[i] = s.Fact
	}
	return out
}

func capFacts(facts []Fact, limit int) []Fact {
	if limit > 0 && len(facts) > limit {
		return facts[:limit]
	}
	return facts
}

// cosine returns the cosine similarity of a and b. Mismatched or empty vectors
// score 0 so they never rank above a genuine match.
func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// vector is a float32 embedding that serializes as a base64-encoded blob of
// little-endian float32s. This keeps the stored record compact (4 bytes per
// dimension) compared with a JSON array of decimal numbers.
type vector []float32

func (v vector) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return json.Marshal(base64.StdEncoding.EncodeToString(buf))
}

func (v *vector) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("memory: decode vector: %w", err)
	}
	if len(raw)%4 != 0 {
		return fmt.Errorf("memory: vector blob length %d not a multiple of 4", len(raw))
	}
	out := make(vector, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	*v = out
	return nil
}
