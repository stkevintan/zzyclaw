package agent

import (
	"context"

	"zzy/copilot"
)

// copilotEmbedder adapts a Copilot client to the Embedder interface, binding a
// fixed embedding model.
type copilotEmbedder struct {
	client *copilot.Client
	model  string
}

// NewCopilotEmbedder returns an Embedder backed by the Copilot embeddings API.
// An empty model uses the client's default embedding model.
func NewCopilotEmbedder(client *copilot.Client, model string) Embedder {
	return copilotEmbedder{client: client, model: model}
}

func (e copilotEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.client.Embeddings(ctx, e.model, texts)
}
