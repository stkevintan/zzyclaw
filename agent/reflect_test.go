package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"zzy/copilot"
)

func reflectTestMessages() []copilot.Message {
	return []copilot.Message{
		{Role: roleUser, Content: "I prefer concise answers and use Go."},
		{Role: roleAssistant, Content: "Got it."},
		{Role: roleUser, Content: "Project is a wechat bot."},
		{Role: roleAssistant, Content: "Noted."},
	}
}

func newTestReflector(store Store, mem StructuralMemory, extract func(ctx context.Context, transcript, existing string) (reflectionResult, error)) *Reflector {
	r := NewReflector(context.Background(), store, mem, nil, ReflectorConfig{
		IdleDelay:   15 * time.Millisecond,
		MinMessages: 2,
	})
	r.extract = extract
	return r
}

func TestReflectorRunsOnIdle(t *testing.T) {
	mem := NewStoreStructuralMemory(NewInMemoryStore(), NewEmbedSemantics(fakeEmbedder{}))
	store := NewInMemoryStore()
	done := make(chan struct{}, 1)
	r := newTestReflector(store, mem, func(_ context.Context, _, _ string) (reflectionResult, error) {
		defer func() { done <- struct{}{} }()
		return reflectionResult{
			Personal: []reflectItem{{Index: "prefers concise answers", Detail: "likes brevity"}},
			Project:  []reflectItem{{Index: "wechat bot project", Detail: "go"}},
		}, nil
	})
	defer r.Stop()

	r.Schedule("u1", "wechat-u1:1", reflectTestMessages())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reflection did not run")
	}
	time.Sleep(20 * time.Millisecond)
	all, _ := mem.List(context.Background(), "u1")
	if len(all) != 2 {
		t.Fatalf("want 2 memories, got %d", len(all))
	}
}

func TestReflectorWatermarkSkipsUnchanged(t *testing.T) {
	mem := NewStoreStructuralMemory(NewInMemoryStore(), NewEmbedSemantics(fakeEmbedder{}))
	store := NewInMemoryStore()
	var mu sync.Mutex
	calls := 0
	fired := make(chan struct{}, 8)
	r := newTestReflector(store, mem, func(_ context.Context, _, _ string) (reflectionResult, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		fired <- struct{}{}
		return reflectionResult{Personal: []reflectItem{{Index: "x", Detail: "y"}}}, nil
	})
	defer r.Stop()

	msgs := reflectTestMessages()
	r.Schedule("u1", "wechat-u1:1", msgs)
	<-fired
	time.Sleep(20 * time.Millisecond)
	// Same history again: hash watermark should short-circuit before extract.
	r.Schedule("u1", "wechat-u1:1", msgs)
	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("want 1 extract call, got %d", calls)
	}
}

func TestReflectorBelowMinMessages(t *testing.T) {
	mem := NewStoreStructuralMemory(NewInMemoryStore(), NewEmbedSemantics(fakeEmbedder{}))
	r := newTestReflector(NewInMemoryStore(), mem, func(context.Context, string, string) (reflectionResult, error) {
		t.Fatal("extract should not be called below min messages")
		return reflectionResult{}, nil
	})
	defer r.Stop()
	r.Schedule("u1", "wechat-u1:1", []copilot.Message{{Role: roleUser, Content: "hi"}})
	time.Sleep(60 * time.Millisecond)
}
