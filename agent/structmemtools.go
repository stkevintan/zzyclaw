package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"zzy/agent/tools"
)

// StructuralMemoryTools builds the structural-memory tools (remember/recall/
// forget) bound to mem. Every operation is scoped to the active user from the
// context, so a user only ever reads or changes their own memory. Reflection
// fills memory automatically; these give the model explicit control too.
func StructuralMemoryTools(mem StructuralMemory) []tools.Tool {
	return []tools.Tool{
		&rememberTool{mem: mem},
		&recallTool{mem: mem},
		&forgetTool{mem: mem},
	}
}

type rememberTool struct{ mem StructuralMemory }

func (t *rememberTool) Name() string { return "remember" }
func (t *rememberTool) Description() string {
	return "Save a durable point to structural memory under one of four categories: personal (the user's character/preferences/role), feedback (their choices/corrections), project (current project facts), reference (other reusable facts). index is a short one-line summary; detail is fuller context."
}
func (t *rememberTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"category":{"type":"string","enum":["personal","feedback","project","reference"],"description":"Which bucket this point belongs to."},"index":{"type":"string","description":"Short, self-contained one-line summary."},"detail":{"type":"string","description":"Fuller context; optional."}},"required":["category","index"]}`)
}
func (t *rememberTool) Dangerous(context.Context, json.RawMessage) bool { return false }
func (t *rememberTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Category string `json:"category"`
		Index    string `json:"index"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	cat := MemoryCategory(strings.ToLower(strings.TrimSpace(a.Category)))
	if !cat.Valid() {
		return "", fmt.Errorf("category must be one of personal, feedback, project, reference")
	}
	if strings.TrimSpace(a.Index) == "" {
		return "", fmt.Errorf("index must not be empty")
	}
	e, err := t.mem.Upsert(ctx, userIDFromContext(ctx), cat, a.Index, a.Detail)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Remembered (id %s, %s): %s", e.ID, e.Category, e.Index), nil
}

type recallTool struct{ mem StructuralMemory }

func (t *recallTool) Name() string { return "recall" }
func (t *recallTool) Description() string {
	return "Search structural memory for points about the current user/project. Returns the most relevant entries with their ids, category and full detail. Empty query returns recent entries."
}
func (t *recallTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to look for. Empty returns the most recent entries."},"limit":{"type":"integer","description":"Maximum number of entries to return (default 5)."}},"required":[]}`)
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
	entries, err := t.mem.Search(ctx, userIDFromContext(ctx), a.Query, a.Limit)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "No matching memories.", nil
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] (%s) %s — %s (%s)\n", e.ID, e.Category, e.Index, e.Detail, e.UpdatedAt.Format(time.DateOnly))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

type forgetTool struct{ mem StructuralMemory }

func (t *forgetTool) Name() string { return "forget" }
func (t *forgetTool) Description() string {
	return "Delete a structural-memory entry by its id (obtain ids from recall). Only the current user's own memory is affected."
}
func (t *forgetTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"The id of the entry to delete, as shown by recall."}},"required":["id"]}`)
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
