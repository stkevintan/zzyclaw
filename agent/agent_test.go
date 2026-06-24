package agent

import (
	"context"
	"testing"
	"zzy/copilot"
)

func TestInMemoryStoreRoundTrip(t *testing.T) {
	s := NewInMemoryStore()
	ctx := context.Background()
	history := []copilot.Message{
		{Role: roleUser, Content: "hi"},
		{Role: roleAssistant, Content: "hello"},
	}
	if err := s.Save(ctx, "k", history); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.Load(ctx, "k")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 || got[0].Content != "hi" || got[1].Content != "hello" {
		t.Fatalf("unexpected history: %+v", got)
	}
	if err := s.Clear(ctx, "k"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = s.Load(ctx, "k")
	if len(got) != 0 {
		t.Fatalf("expected empty after clear, got %+v", got)
	}
}

func TestSessionManagerMultiSessionIsolation(t *testing.T) {
	ctx := context.Background()
	m := NewSessionManager(NewInMemoryStore())

	// Each user starts with one default current session, isolated by key.
	a1 := m.Current(ctx, "alice")
	b1 := m.Current(ctx, "bob")
	if a1.Key == b1.Key {
		t.Fatalf("different users share a session key: %s", a1.Key)
	}

	// Alice creates a second session; it becomes current and is distinct.
	a2 := m.New(ctx, "alice")
	if a2.ID == a1.ID || a2.Key == a1.Key {
		t.Fatalf("new session not distinct: %s vs %s", a1.ID, a2.ID)
	}
	if cur := m.Current(ctx, "alice"); cur.ID != a2.ID {
		t.Fatalf("expected current to be new session %s, got %s", a2.ID, cur.ID)
	}
	metas, current := m.List(ctx, "alice")
	if len(metas) != 2 || current != a2.ID {
		t.Fatalf("unexpected list: metas=%d current=%s", len(metas), current)
	}

	// Bob is unaffected by Alice's sessions.
	if bMetas, _ := m.List(ctx, "bob"); len(bMetas) != 1 {
		t.Fatalf("bob should have exactly one session, got %d", len(bMetas))
	}

	// Selecting switches the current session back.
	if sel, err := m.Select(ctx, "alice", a1.ID); err != nil || sel.ID != a1.ID {
		t.Fatalf("select first session: sel=%v err=%v", sel, err)
	}

	// Deleting the current session falls back to another, never leaving zero.
	next, err := m.Delete(ctx, "alice", a1.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if next.ID == a1.ID {
		t.Fatalf("delete returned the removed session")
	}
	if metas, _ := m.List(ctx, "alice"); len(metas) != 1 {
		t.Fatalf("expected one session after delete, got %d", len(metas))
	}
}

func TestParseDecision(t *testing.T) {
	cases := map[string]struct {
		decision Decision
		ok       bool
	}{
		"yes":    {DecisionApprove, true},
		"y":      {DecisionApprove, true},
		"确认":     {DecisionApprove, true},
		"always": {DecisionAlways, true},
		"始终":     {DecisionAlways, true},
		"记住":     {DecisionAlways, true},
		"no":     {DecisionDeny, true},
		"取消":     {DecisionDeny, true},
		"maybe":  {DecisionDeny, false},
		"":       {DecisionDeny, false},
	}
	for in, want := range cases {
		decision, ok := parseDecision(in)
		if decision != want.decision || ok != want.ok {
			t.Errorf("parseDecision(%q) = (%v,%v), want (%v,%v)", in, decision, ok, want.decision, want.ok)
		}
	}
}

func TestTrimHistoryCapsAndAlignsToUser(t *testing.T) {
	msgs := []copilot.Message{
		{Role: roleAssistant, ToolCalls: []copilot.ToolCall{{ID: "1"}}},
		{Role: roleTool, ToolCallID: "1", Content: "result"},
		{Role: roleUser, Content: "question"},
		{Role: roleAssistant, Content: "answer"},
	}
	out := trimHistory(msgs, 3)
	if len(out) == 0 {
		t.Fatal("expected non-empty history")
	}
	if out[0].Role != roleUser {
		t.Fatalf("history should start at a user message, got role %q", out[0].Role)
	}
}

func TestTrimHistoryUnderCap(t *testing.T) {
	msgs := []copilot.Message{
		{Role: roleUser, Content: "a"},
		{Role: roleAssistant, Content: "b"},
	}
	out := trimHistory(msgs, 40)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
}

func TestOwnerGate(t *testing.T) {
	// With owners configured, only listed users may run dangerous tools.
	e := NewEngine(nil, nil, nil, nil, EngineConfig{Owners: []string{"alice", ""}})
	if !e.ownerAllowed(&Session{UserID: "alice"}) {
		t.Error("owner alice should be allowed")
	}
	if e.ownerAllowed(&Session{UserID: "mallory"}) {
		t.Error("non-owner mallory should be denied")
	}
	if e.ownerAllowed(&Session{UserID: ""}) {
		t.Error("empty user ID should never match an owner")
	}

	// With no owners, the gate is disabled and everyone is allowed.
	open := NewEngine(nil, nil, nil, nil, EngineConfig{})
	if !open.ownerAllowed(&Session{UserID: "anyone"}) {
		t.Error("empty owners should allow everyone")
	}
}
