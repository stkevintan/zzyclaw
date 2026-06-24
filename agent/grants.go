package agent

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
)

// GrantStore persists per-user "always approve" decisions so repeated,
// equivalent dangerous actions don't prompt again. Grants are isolated per user:
// one user's approvals never apply to another. Keys are opaque scope identifiers
// produced by tools (see tools.Grantable), e.g. a network host or a workspace
// directory.
type GrantStore interface {
	// Allowed reports whether userID has granted key.
	Allowed(ctx context.Context, userID, key string) bool
	// Grant remembers key for userID so future Allowed calls return true.
	Grant(ctx context.Context, userID, key string) error
}

// noopGrantStore never remembers anything; every dangerous call is prompted.
// Used when no persistent store is configured (e.g. in tests).
type noopGrantStore struct{}

func (noopGrantStore) Allowed(context.Context, string, string) bool { return false }
func (noopGrantStore) Grant(context.Context, string, string) error  { return nil }

// NewNoopGrantStore returns a GrantStore that never remembers approvals.
func NewNoopGrantStore() GrantStore { return noopGrantStore{} }

// storeGrantStore persists per-user grant sets through the shared Store (Redis
// in production, in-memory in dev), mirroring how per-user session indexes are
// stored. Each user's set is cached in memory after first access to keep the
// Allowed hot path off the network.
type storeGrantStore struct {
	store Store

	mu    sync.Mutex
	cache map[string]map[string]struct{} // userID -> granted keys (hydrated lazily)
}

// NewStoreGrantStore returns a per-user GrantStore backed by store. It shares the
// same durability as conversation memory and session indexes: grants survive
// restarts when the store is Redis-backed, and are process-local otherwise.
func NewStoreGrantStore(store Store) GrantStore {
	return &storeGrantStore{store: store, cache: make(map[string]map[string]struct{})}
}

func grantsMetaKey(userID string) string { return "grants:" + userID }

type grantsRecord struct {
	Keys []string `json:"keys"`
}

// userSetLocked returns the hydrated grant set for userID, loading it from the
// store on first access. The caller must hold s.mu.
func (s *storeGrantStore) userSetLocked(ctx context.Context, userID string) map[string]struct{} {
	if set, ok := s.cache[userID]; ok {
		return set
	}
	set := make(map[string]struct{})
	if data, err := s.store.GetMeta(ctx, grantsMetaKey(userID)); err == nil && len(data) > 0 {
		var rec grantsRecord
		if json.Unmarshal(data, &rec) == nil {
			for _, k := range rec.Keys {
				if k != "" {
					set[k] = struct{}{}
				}
			}
		}
	}
	s.cache[userID] = set
	return set
}

func (s *storeGrantStore) Allowed(ctx context.Context, userID, key string) bool {
	if userID == "" || key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.userSetLocked(ctx, userID)[key]
	return ok
}

func (s *storeGrantStore) Grant(ctx context.Context, userID, key string) error {
	if userID == "" || key == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.userSetLocked(ctx, userID)
	if _, ok := set[key]; ok {
		return nil
	}
	set[key] = struct{}{}
	return s.persistLocked(ctx, userID, set)
}

// persistLocked writes a user's grant set to the store. The caller must hold s.mu.
func (s *storeGrantStore) persistLocked(ctx context.Context, userID string, set map[string]struct{}) error {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	data, err := json.Marshal(grantsRecord{Keys: keys})
	if err != nil {
		return err
	}
	return s.store.SetMeta(ctx, grantsMetaKey(userID), data)
}
