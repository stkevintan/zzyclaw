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
	"strings"
	"time"
)

// Embedder turns text into vectors for semantic similarity search. One vector is
// returned per input string, in input order. It is the only external dependency
// of the memory layer; in production it is backed by the Copilot embeddings API.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// RankItem is one candidate note: its Index (the short summary line) and, when
// available, its precomputed embedding. Vector strategies use Vec (embedding only
// the query, not every candidate); text/LLM strategies use Index.
type RankItem struct {
	Index string
	Vec   []float32
}

// MemoSemantics is the single abstraction the memory store depends on for the
// operations that would otherwise require a concrete embedder: detecting
// duplicates and ranking by relevance. Implementations back these with
// embeddings, an LLM, or a composite of both — merging the two strategies behind
// one interface so the store never touches an Embedder itself.
type MemoSemantics interface {
	// Dedup decides whether note duplicates one of candidates: it returns the
	// position of the candidate to merge into (or -1 to store separately) and the
	// opaque vector to persist with note (nil when embeddings are unavailable).
	// It encapsulates the strategy — embedding cosine or a small LLM — so callers
	// never embed or compare vectors themselves. A non-nil error means it could
	// not decide, so a composite can fall back to another strategy. Passing no
	// candidates yields just the vector (match is always -1), which callers use to
	// vectorize a note for storage.
	Dedup(ctx context.Context, note string, candidates []RankItem) (match int, vec []float32, err error)
	// Rank orders candidates by relevance to query, best first, up to n. It returns
	// a non-nil error when it cannot rank (e.g. the query could not be embedded).
	Rank(ctx context.Context, query string, candidates []RankItem, n int) ([]int, error)
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

// newID returns a short random hex identifier. Randomness (rather than a
// counter) avoids cross-process collisions when the store is shared.
func newID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read essentially never fails; fall back to a timestamp.
		return strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	}
	return hex.EncodeToString(b[:])
}
