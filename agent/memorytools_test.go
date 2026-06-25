package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"zzy/agent/tools"
)

// memToolByName finds a memory tool by its registered name.
func memToolByName(ts []tools.Tool, name string) tools.Tool {
	for _, t := range ts {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func TestMemoryToolsRememberRecallForget(t *testing.T) {
	mem := NewStoreUserMemory(NewInMemoryStore(), fakeEmbedder{})
	ts := MemoryTools(mem)
	ctx := withSession(context.Background(), &Session{UserID: "u1"})

	remember := memToolByName(ts, "remember")
	recall := memToolByName(ts, "recall")
	forget := memToolByName(ts, "forget")
	if remember == nil || recall == nil || forget == nil {
		t.Fatal("MemoryTools must provide remember, recall and forget")
	}

	// None of the memory tools are approval-gated (memory is per-user isolated).
	for _, tool := range ts {
		if tool.Dangerous(ctx, nil) {
			t.Errorf("%s must not be dangerous", tool.Name())
		}
	}

	if _, err := remember.Execute(ctx, json.RawMessage(`{"text":"Prefers email over phone"}`)); err != nil {
		t.Fatalf("remember: %v", err)
	}

	out, err := recall.Execute(ctx, json.RawMessage(`{"query":"email"}`))
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !strings.Contains(out, "Prefers email over phone") {
		t.Fatalf("recall output = %q, want the remembered fact", out)
	}

	// Pull the fact id back out of memory to drive forget.
	facts, _ := mem.List(ctx, "u1")
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	id := facts[0].ID

	if _, err := forget.Execute(ctx, json.RawMessage(`{"id":"`+id+`"}`)); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if facts, _ := mem.List(ctx, "u1"); len(facts) != 0 {
		t.Errorf("fact not forgotten: %d remain", len(facts))
	}
}

func TestRememberRejectsBlank(t *testing.T) {
	ts := MemoryTools(NewStoreUserMemory(NewInMemoryStore(), fakeEmbedder{}))
	ctx := withSession(context.Background(), &Session{UserID: "u1"})
	if _, err := memToolByName(ts, "remember").Execute(ctx, json.RawMessage(`{"text":"  "}`)); err == nil {
		t.Error("expected error remembering blank text")
	}
}

func TestForgetUnknownIDReports(t *testing.T) {
	ts := MemoryTools(NewStoreUserMemory(NewInMemoryStore(), fakeEmbedder{}))
	ctx := withSession(context.Background(), &Session{UserID: "u1"})
	out, err := memToolByName(ts, "forget").Execute(ctx, json.RawMessage(`{"id":"nope"}`))
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if !strings.Contains(out, "No memory") {
		t.Errorf("forget unknown id = %q, want a not-found message", out)
	}
}
