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
	// before it runs (e.g. writing or deleting files, executing scripts). The
	// context carries the active user, so per-user resources can be inspected.
	Dangerous(ctx context.Context, args json.RawMessage) bool
	// Execute runs the tool with the given JSON-encoded arguments and returns a
	// human/model-readable result string.
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Grantable is an optional interface a Tool can implement to support persistent,
// scope-based approvals ("always allow"). When the user approves a dangerous
// call with "always", the returned key is remembered so future calls with the
// same scope skip the approval prompt.
type Grantable interface {
	// GrantScope returns a stable key identifying the resource a specific call
	// touches (e.g. a network host or a workspace directory) plus a short human
	// label for the prompt. The context carries the active user so per-user
	// resources can be resolved. ok=false means the call cannot be remembered and
	// must be approved every time.
	GrantScope(ctx context.Context, args json.RawMessage) (key, label string, ok bool)
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
