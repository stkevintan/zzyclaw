package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"
	"zzy/copilot"

	"github.com/redis/go-redis/v9"
)

// Store persists per-session conversation history and per-user metadata.
// Implementations must be safe for concurrent use by multiple sessions.
type Store interface {
	// Load returns the stored history for key, or nil if none exists.
	Load(ctx context.Context, key string) ([]copilot.Message, error)
	// Save replaces the stored history for key.
	Save(ctx context.Context, key string, history []copilot.Message) error
	// Clear removes any stored history for key.
	Clear(ctx context.Context, key string) error
	// GetMeta returns arbitrary metadata bytes for key (e.g. a user's session
	// index), or nil if none exists.
	GetMeta(ctx context.Context, key string) ([]byte, error)
	// SetMeta stores arbitrary metadata bytes for key (never expires).
	SetMeta(ctx context.Context, key string, value []byte) error
}

// InMemoryStore keeps conversation history in process memory. History is lost
// when the program exits; suitable when Redis is not configured.
type InMemoryStore struct {
	mu   sync.Mutex
	data map[string][]copilot.Message
	meta map[string][]byte
}

// NewInMemoryStore creates an empty in-memory store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		data: make(map[string][]copilot.Message),
		meta: make(map[string][]byte),
	}
}

func (s *InMemoryStore) Load(_ context.Context, key string) ([]copilot.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.data[key]
	return append([]copilot.Message(nil), h...), nil
}

func (s *InMemoryStore) Save(_ context.Context, key string, history []copilot.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = append([]copilot.Message(nil), history...)
	return nil
}

func (s *InMemoryStore) Clear(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *InMemoryStore) GetMeta(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.meta[key]...), nil
}

func (s *InMemoryStore) SetMeta(_ context.Context, key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta[key] = append([]byte(nil), value...)
	return nil
}

// RedisStore persists conversation history in Redis as JSON, optionally with a
// per-key TTL.
type RedisStore struct {
	client *redis.Client
	ttl    time.Duration
	prefix string
}

// NewRedisStore connects to Redis and verifies connectivity with a ping.
func NewRedisStore(ctx context.Context, addr, password string, db int, ttl time.Duration) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &RedisStore{client: client, ttl: ttl, prefix: "agent:mem:"}, nil
}

func (s *RedisStore) key(k string) string { return s.prefix + k }

func (s *RedisStore) Load(ctx context.Context, key string) ([]copilot.Message, error) {
	data, err := s.client.Get(ctx, s.key(key)).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	var history []copilot.Message
	if err := json.Unmarshal(data, &history); err != nil {
		return nil, err
	}
	return history, nil
}

func (s *RedisStore) Save(ctx context.Context, key string, history []copilot.Message) error {
	data, err := json.Marshal(history)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.key(key), data, s.ttl).Err()
}

func (s *RedisStore) Clear(ctx context.Context, key string) error {
	return s.client.Del(ctx, s.key(key)).Err()
}

func (s *RedisStore) metaKey(k string) string { return "agent:idx:" + k }

func (s *RedisStore) GetMeta(ctx context.Context, key string) ([]byte, error) {
	data, err := s.client.Get(ctx, s.metaKey(key)).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

func (s *RedisStore) SetMeta(ctx context.Context, key string, value []byte) error {
	// Session index must persist, so it is stored without a TTL.
	return s.client.Set(ctx, s.metaKey(key), value, 0).Err()
}
