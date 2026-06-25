package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"zzy/agent/tools"
)

// MemoryTools builds the long-term memory tools (remember/recall/forget) bound
// to mem. Every operation is scoped to the active user taken from the context,
// so a user can only ever read or change their own memory. Because memory is
// fully per-user isolated (like a user's private skills), these tools are not
// approval-gated; the explicit, id-targeted forget keeps deletion deliberate.
func MemoryTools(mem UserMemory) []tools.Tool {
	return []tools.Tool{
		&rememberTool{mem: mem},
		&recallTool{mem: mem},
		&forgetTool{mem: mem},
	}
}

type rememberTool struct{ mem UserMemory }

func (t *rememberTool) Name() string { return "remember" }
func (t *rememberTool) Description() string {
	return "Save a durable fact to long-term memory so it can be recalled in future conversations with this user. Use it for stable, reusable information (the user's preferences, recurring context, important decisions) — not transient, task-specific details. State the fact as a self-contained sentence."
}
func (t *rememberTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string","description":"The fact to remember, written as a self-contained sentence."}},"required":["text"]}`)
}
func (t *rememberTool) Dangerous(context.Context, json.RawMessage) bool { return false }
func (t *rememberTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Text) == "" {
		return "", fmt.Errorf("text must not be empty")
	}
	f, err := t.mem.Add(ctx, userIDFromContext(ctx), a.Text)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Remembered (id %s): %s", f.ID, f.Text), nil
}

type recallTool struct{ mem UserMemory }

func (t *recallTool) Name() string { return "recall" }
func (t *recallTool) Description() string {
	return "Search your long-term memory for facts about the current user. Returns the most relevant remembered facts with their ids. Use an empty query to list the most recent facts."
}
func (t *recallTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to look for. Empty returns the most recent facts."},"limit":{"type":"integer","description":"Maximum number of facts to return (default 5)."}},"required":[]}`)
}
func (t *recallTool) Dangerous(context.Context, json.RawMessage) bool { return false }
func (t *recallTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	facts, err := t.mem.Search(ctx, userIDFromContext(ctx), a.Query, a.Limit)
	if err != nil {
		return "", err
	}
	if len(facts) == 0 {
		return "No matching memories.", nil
	}
	var b strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&b, "- [%s] %s (%s)\n", f.ID, f.Text, f.CreatedAt.Format(time.DateOnly))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

type forgetTool struct{ mem UserMemory }

func (t *forgetTool) Name() string { return "forget" }
func (t *forgetTool) Description() string {
	return "Delete a remembered fact from long-term memory by its id (obtain ids from recall). Only the current user's own memory is affected."
}
func (t *forgetTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"The id of the fact to delete, as shown by recall."}},"required":["id"]}`)
}
func (t *forgetTool) Dangerous(context.Context, json.RawMessage) bool { return false }
func (t *forgetTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.ID) == "" {
		return "", fmt.Errorf("id must not be empty")
	}
	found, err := t.mem.Delete(ctx, userIDFromContext(ctx), a.ID)
	if err != nil {
		return "", err
	}
	if !found {
		return fmt.Sprintf("No memory with id %q.", a.ID), nil
	}
	return fmt.Sprintf("Forgot memory %s.", a.ID), nil
}
