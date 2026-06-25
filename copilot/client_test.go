package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEmbeddings(t *testing.T) {
	var gotIntegration, gotAuth string
	var gotReq embeddingsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		gotIntegration = r.Header.Get("Copilot-Integration-Id")
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		// Return data out of order to verify the client reorders by index.
		resp := embeddingsResponse{Data: []embeddingDatum{
			{Index: 1, Embedding: []float32{0, 1}},
			{Index: 0, Embedding: []float32{1, 0}},
		}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient("gh-token")
	c.baseURL = srv.URL
	c.apiToken = "api-token" // skip token exchange
	c.expiresAt = time.Now().Add(time.Hour)

	out, err := c.Embeddings(context.Background(), "", []string{"first", "second"})
	if err != nil {
		t.Fatalf("Embeddings: %v", err)
	}
	if len(out) != 2 || out[0][0] != 1 || out[1][1] != 1 {
		t.Fatalf("vectors not ordered by index: %v", out)
	}
	if gotIntegration != "vscode-chat" {
		t.Errorf("Copilot-Integration-Id = %q, want vscode-chat", gotIntegration)
	}
	if gotAuth != "Bearer api-token" {
		t.Errorf("Authorization = %q, want Bearer api-token", gotAuth)
	}
	if gotReq.Model != defaultEmbeddingModel {
		t.Errorf("model = %q, want default %q", gotReq.Model, defaultEmbeddingModel)
	}
}

func TestEmbeddingsCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := embeddingsResponse{Data: []embeddingDatum{{Index: 0, Embedding: []float32{1}}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient("gh-token")
	c.baseURL = srv.URL
	c.apiToken = "api-token"
	c.expiresAt = time.Now().Add(time.Hour)

	if _, err := c.Embeddings(context.Background(), "m", []string{"a", "b"}); err == nil {
		t.Error("expected error when response vector count != input count")
	}
}
