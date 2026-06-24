// Package tools defines the agent's pluggable tool interface and a registry of
// built-in tools (filesystem access, script execution). Tools are the concrete
// capabilities the ReAct engine exposes to the model via native function calling.
package tools

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
)

// Tool is a single capability the agent may invoke. Implementations must be safe
// for concurrent use: a tool can be executed by multiple sessions at once.
type Tool interface {
	// Name is the unique function name exposed to the model.
	Name() string
	// Description tells the model what the tool does and when to use it.
	Description() string
	// Schema returns the JSON schema describing the tool's parameters.
	Schema() json.RawMessage
	// Dangerous reports whether a given invocation requires human approval
	// before it runs (e.g. writing or deleting files, executing scripts).
	Dangerous(args json.RawMessage) bool
	// Execute runs the tool with the given JSON-encoded arguments and returns a
	// human/model-readable result string.
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry is a concurrency-safe collection of tools keyed by name.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds (or replaces) a tool in the registry.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get returns the tool registered under name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// All returns every registered tool, sorted by name for stable output.
func (r *Registry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
