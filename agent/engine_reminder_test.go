package agent

import (
	"context"
	"strings"
	"testing"

	"zzy/agent/skill"
	"zzy/agent/tools"
	"zzy/copilot"
)

func TestStructReminderRendersAndScopes(t *testing.T) {
	mem := NewStoreStructuralMemory(NewInMemoryStore(), fakeEmbedder{})
	ctx := context.Background()
	mustUpsert(t, mem, ctx, "u1", CategoryPersonal, "prefers concise answers", "brevity")
	mustUpsert(t, mem, ctx, "u1", CategoryProject, "wechat bot in go", "go 1.25")

	e := &Engine{structMem: mem, structInject: 6}
	sess := &Session{UserID: "u1"}
	msgs := []copilot.Message{{Role: roleUser, Content: "tell me about the project"}}

	rem := e.structReminder(ctx, sess, msgs)
	if !strings.Contains(rem, "<system-reminder>") || !strings.Contains(rem, "## Personal") || !strings.Contains(rem, "## Project") {
		t.Fatalf("reminder missing structure: %q", rem)
	}
	if !strings.Contains(rem, "prefers concise answers") {
		t.Fatalf("reminder missing index: %q", rem)
	}

	// A user with no memory gets nothing.
	if r := e.structReminder(ctx, &Session{UserID: "u2"}, msgs); r != "" {
		t.Fatalf("expected empty reminder for u2, got %q", r)
	}
}

func TestFullMessagesInsertsReminderBeforeLastUser(t *testing.T) {
	mem := NewStoreStructuralMemory(NewInMemoryStore(), fakeEmbedder{})
	mustUpsert(t, mem, context.Background(), "u1", CategoryPersonal, "prefers go", "x")
	mgr, err := skill.NewManager(t.TempDir(), func(string) (string, error) { return t.TempDir(), nil })
	if err != nil {
		t.Fatalf("skill manager: %v", err)
	}
	e := NewEngine(nil, tools.NewRegistry(), mgr, NewInMemoryStore(), EngineConfig{StructMem: mem, StructInject: 6})
	sess := &Session{UserID: "u1"}
	msgs := []copilot.Message{
		{Role: roleUser, Content: "first"},
		{Role: roleAssistant, Content: "ok"},
		{Role: roleUser, Content: "latest"},
	}
	out := e.fullMessages(context.Background(), sess, msgs)
	// system prompt + reminder + 3 = 5; reminder sits right before "latest".
	if len(out) != 5 {
		t.Fatalf("want 5 messages, got %d", len(out))
	}
	if !strings.Contains(out[3].Content, "<system-reminder>") || out[4].Content != "latest" {
		t.Fatalf("reminder not before last user; got [3]=%q [4]=%q", out[3].Content, out[4].Content)
	}
}
