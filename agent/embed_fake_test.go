package agent

import (
	"context"
	"fmt"
	"strings"
)

// fakeEmbedder is a deterministic, offline stand-in for the Copilot embeddings
// API. It encodes a bag of hashed words, so cosine similarity reflects shared
// vocabulary — enough to exercise ranking, ordering, and persistence without a
// network call.
type fakeEmbedder struct{}

const fakeEmbedDim = 256

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, fakeEmbedDim)
		for _, w := range strings.Fields(strings.ToLower(t)) {
			h := uint32(2166136261)
			for j := 0; j < len(w); j++ {
				h ^= uint32(w[j])
				h *= 16777619
			}
			v[h%fakeEmbedDim]++
		}
		out[i] = v
	}
	return out, nil
}

// errEmbedder always fails, to exercise the error paths.
type errEmbedder struct{}

func (errEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, fmt.Errorf("embed boom")
}
