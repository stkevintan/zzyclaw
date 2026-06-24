package agent

import (
	"context"
	"testing"
)

func TestStoreGrantStorePerUserIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewStoreGrantStore(NewInMemoryStore())

	if err := s.Grant(ctx, "alice", "http_get:example.com"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !s.Allowed(ctx, "alice", "http_get:example.com") {
		t.Fatal("alice's grant should be allowed for alice")
	}
	// A different user must not inherit alice's grant.
	if s.Allowed(ctx, "bob", "http_get:example.com") {
		t.Fatal("bob must not see alice's grant")
	}
	// Unrelated key for the same user is not allowed.
	if s.Allowed(ctx, "alice", "http_get:other.com") {
		t.Fatal("unrelated key must not be allowed")
	}
}

func TestStoreGrantStorePersistsAcrossInstances(t *testing.T) {
	ctx := context.Background()
	backing := NewInMemoryStore() // shared backing store across two grant stores

	s1 := NewStoreGrantStore(backing)
	if err := s1.Grant(ctx, "alice", "fs_write:/data/agent/workspace"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// A fresh grant store over the same backing store (e.g. after a restart with
	// Redis) sees the persisted grant.
	s2 := NewStoreGrantStore(backing)
	if !s2.Allowed(ctx, "alice", "fs_write:/data/agent/workspace") {
		t.Fatal("persisted grant should be visible to a new grant store instance")
	}
}

func TestStoreGrantStoreGrantIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := NewStoreGrantStore(NewInMemoryStore())
	for i := 0; i < 3; i++ {
		if err := s.Grant(ctx, "alice", "http_get:example.com"); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}
	if !s.Allowed(ctx, "alice", "http_get:example.com") {
		t.Fatal("grant should be allowed after repeated grants")
	}
}

func TestNoopGrantStore(t *testing.T) {
	ctx := context.Background()
	s := NewNoopGrantStore()
	if err := s.Grant(ctx, "alice", "anything"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if s.Allowed(ctx, "alice", "anything") {
		t.Fatal("noop store must never remember grants")
	}
}

func TestGrantEmptyInputsAreNoops(t *testing.T) {
	ctx := context.Background()
	s := NewStoreGrantStore(NewInMemoryStore())
	if err := s.Grant(ctx, "", "key"); err != nil {
		t.Fatalf("grant empty user: %v", err)
	}
	if err := s.Grant(ctx, "alice", ""); err != nil {
		t.Fatalf("grant empty key: %v", err)
	}
	if s.Allowed(ctx, "", "key") || s.Allowed(ctx, "alice", "") {
		t.Fatal("empty userID or key must never be allowed")
	}
}
