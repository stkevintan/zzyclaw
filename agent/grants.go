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
//
// Locking is per user: the small global mutex only guards the cache map lookup,
// while all store I/O happens under a user's own lock. This keeps one user's
// (or one slow Redis call's) latency from blocking every other user.
type storeGrantStore struct {
	store Store

	mu    sync.Mutex
	cache map[string]*userGrantSet // userID -> grant set (created lazily)
}

// userGrantSet is one user's cached grant set, loaded from the store at most
// once on success. A failed load leaves loaded=false so the next call retries
// instead of caching an empty set permanently.
type userGrantSet struct {
	mu     sync.RWMutex
	loaded bool
	keys   map[string]struct{}
}

// NewStoreGrantStore returns a per-user GrantStore backed by store. It shares the
// same durability as conversation memory and session indexes: grants survive
// restarts when the store is Redis-backed, and are process-local otherwise.
func NewStoreGrantStore(store Store) GrantStore {
	return &storeGrantStore{store: store, cache: make(map[string]*userGrantSet)}
}

func grantsMetaKey(userID string) string { return "grants:" + userID }

type grantsRecord struct {
	Keys []string `json:"keys"`
}

// userSet returns the per-user grant set, creating it on first access. Only the
// global cache-map lookup is guarded here; store I/O happens later under the
// user's own lock (see ensureLoaded).
func (s *storeGrantStore) userSet(userID string) *userGrantSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	us, ok := s.cache[userID]
	if !ok {
		us = &userGrantSet{keys: make(map[string]struct{})}
		s.cache[userID] = us
	}
	return us
}

// ensureLoaded hydrates us from the store once. A transient store error leaves
// us.loaded false so a later call retries rather than caching an empty set.
func (s *storeGrantStore) ensureLoaded(ctx context.Context, userID string, us *userGrantSet) {
	us.mu.RLock()
	loaded := us.loaded
	us.mu.RUnlock()
	if loaded {
		return
	}

	us.mu.Lock()
	defer us.mu.Unlock()
	if us.loaded {
		return
	}
	data, err := s.store.GetMeta(ctx, grantsMetaKey(userID))
	if err != nil {
		// Leave loaded=false to retry on the next call.
		return
	}
	if len(data) > 0 {
		var rec grantsRecord
		if json.Unmarshal(data, &rec) == nil {
			for _, k := range rec.Keys {
				if k != "" {
					us.keys[k] = struct{}{}
				}
			}
		}
	}
	us.loaded = true
}

func (s *storeGrantStore) Allowed(ctx context.Context, userID, key string) bool {
	if userID == "" || key == "" {
		return false
	}
	us := s.userSet(userID)
	s.ensureLoaded(ctx, userID, us)

	us.mu.RLock()
	defer us.mu.RUnlock()
	_, ok := us.keys[key]
	return ok
}

func (s *storeGrantStore) Grant(ctx context.Context, userID, key string) error {
	if userID == "" || key == "" {
		return nil
	}
	us := s.userSet(userID)
	s.ensureLoaded(ctx, userID, us)

	us.mu.Lock()
	defer us.mu.Unlock()
	if _, ok := us.keys[key]; ok {
		return nil
	}
	us.keys[key] = struct{}{}

	keys := make([]string, 0, len(us.keys))
	for k := range us.keys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	data, err := json.Marshal(grantsRecord{Keys: keys})
	if err != nil {
		return err
	}
	return s.store.SetMeta(ctx, grantsMetaKey(userID), data)
}
