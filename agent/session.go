package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"zzy/copilot"
)

// PendingApproval captures the state of a ReAct turn that is paused waiting for
// the user to approve (or deny) a dangerous tool call.
type PendingApproval struct {
	// Messages is the conversation built so far, including the assistant message
	// that requested the tool call and any tool results already produced.
	Messages []copilot.Message
	// Call is the dangerous tool call awaiting a decision.
	Call copilot.ToolCall
	// Queue holds the remaining tool calls from the same assistant message that
	// have not been processed yet.
	Queue []copilot.ToolCall
	// Description is the human-readable summary shown to the user.
	Description string
	// GrantKey, when non-empty, is the scope that "always" approval will persist
	// so future equivalent calls skip the prompt (see tools.Grantable).
	GrantKey string
	// GrantLabel is the human-readable form of GrantKey shown in the prompt.
	GrantLabel string
}

// Session holds the state of one conversation thread for a user. All access is
// guarded by Mu so a single conversation's turns are serialized.
type Session struct {
	ID     string // per-user session identifier
	Key    string // store key for this session's history
	UserID string // owner of this session (used for permission gating)
	Mu     sync.Mutex

	History      []copilot.Message
	ActiveSkills map[string]struct{}
	Pending      *PendingApproval
}

// SessionMeta is the lightweight, persisted description of a session shown in
// listings.
type SessionMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// userIndex is the persisted set of sessions belonging to a single user.
type userIndex struct {
	Current  string        `json:"current"`
	Seq      int           `json:"seq"`
	Sessions []SessionMeta `json:"sessions"`
}

// userSessions is the in-memory view of one user's sessions, fully isolated
// from every other user.
type userSessions struct {
	mu     sync.Mutex
	userID string
	index  userIndex
	live   map[string]*Session
}

// SessionManager owns all users' sessions. Different users never share history,
// memory or skill state.
type SessionManager struct {
	store Store

	mu    sync.Mutex
	users map[string]*userSessions
}

// NewSessionManager returns a manager backed by the given store.
func NewSessionManager(store Store) *SessionManager {
	return &SessionManager{store: store, users: make(map[string]*userSessions)}
}

const defaultTitle = "新会话"

func historyKey(userID, id string) string { return "wechat-" + userID + ":" + id }
func userIndexKey(userID string) string   { return "wechat-" + userID }

// user returns the (cached) session set for a user, hydrating the index from the
// store on first access.
func (m *SessionManager) user(ctx context.Context, userID string) *userSessions {
	m.mu.Lock()
	defer m.mu.Unlock()
	us, ok := m.users[userID]
	if !ok {
		us = &userSessions{userID: userID, live: make(map[string]*Session)}
		// Hydrate the index before publishing us into the map, so concurrent
		// callers never observe an un-hydrated session and race on us.index.
		if data, err := m.store.GetMeta(ctx, userIndexKey(userID)); err == nil && len(data) > 0 {
			_ = json.Unmarshal(data, &us.index)
		}
		m.users[userID] = us
	}
	return us
}

func (m *SessionManager) saveIndex(ctx context.Context, us *userSessions) {
	data, err := json.Marshal(us.index)
	if err != nil {
		return
	}
	_ = m.store.SetMeta(ctx, userIndexKey(us.userID), data)
}

// --- helpers; caller must hold us.mu ---

func (us *userSessions) hasLocked(id string) bool {
	for i := range us.index.Sessions {
		if us.index.Sessions[i].ID == id {
			return true
		}
	}
	return false
}

func (us *userSessions) createLocked() SessionMeta {
	us.index.Seq++
	meta := SessionMeta{ID: strconv.Itoa(us.index.Seq), Title: defaultTitle, CreatedAt: time.Now()}
	us.index.Sessions = append(us.index.Sessions, meta)
	return meta
}

func (us *userSessions) getOrLoadLocked(ctx context.Context, store Store, id string) *Session {
	if s, ok := us.live[id]; ok {
		return s
	}
	s := &Session{ID: id, Key: historyKey(us.userID, id), UserID: us.userID, ActiveSkills: make(map[string]struct{})}
	if h, err := store.Load(ctx, s.Key); err == nil && len(h) > 0 {
		s.History = h
	}
	us.live[id] = s
	return s
}

// Current returns the user's active session, creating a default one if none
// exists yet.
func (m *SessionManager) Current(ctx context.Context, userID string) *Session {
	us := m.user(ctx, userID)
	us.mu.Lock()
	defer us.mu.Unlock()
	if us.index.Current == "" || !us.hasLocked(us.index.Current) {
		meta := us.createLocked()
		us.index.Current = meta.ID
		m.saveIndex(ctx, us)
	}
	return us.getOrLoadLocked(ctx, m.store, us.index.Current)
}

// List returns the user's sessions and the current session ID.
func (m *SessionManager) List(ctx context.Context, userID string) ([]SessionMeta, string) {
	us := m.user(ctx, userID)
	us.mu.Lock()
	defer us.mu.Unlock()
	out := make([]SessionMeta, len(us.index.Sessions))
	copy(out, us.index.Sessions)
	return out, us.index.Current
}

// New creates a fresh session and makes it current.
func (m *SessionManager) New(ctx context.Context, userID string) *Session {
	us := m.user(ctx, userID)
	us.mu.Lock()
	defer us.mu.Unlock()
	meta := us.createLocked()
	us.index.Current = meta.ID
	m.saveIndex(ctx, us)
	return us.getOrLoadLocked(ctx, m.store, meta.ID)
}

// Select switches the current session to id.
func (m *SessionManager) Select(ctx context.Context, userID, id string) (*Session, error) {
	us := m.user(ctx, userID)
	us.mu.Lock()
	defer us.mu.Unlock()
	if !us.hasLocked(id) {
		return nil, fmt.Errorf("session %s not found", id)
	}
	us.index.Current = id
	m.saveIndex(ctx, us)
	return us.getOrLoadLocked(ctx, m.store, id), nil
}

// Delete removes a session (and its history) and returns the new current
// session, creating one if the user has none left.
func (m *SessionManager) Delete(ctx context.Context, userID, id string) (*Session, error) {
	us := m.user(ctx, userID)
	us.mu.Lock()
	defer us.mu.Unlock()

	idx := -1
	for i := range us.index.Sessions {
		if us.index.Sessions[i].ID == id {
			idx = i
			break
		}
	}
	if idx == -1 {
		return nil, fmt.Errorf("session %s not found", id)
	}

	delete(us.live, id)
	_ = m.store.Clear(ctx, historyKey(userID, id))
	us.index.Sessions = append(us.index.Sessions[:idx], us.index.Sessions[idx+1:]...)

	if us.index.Current == id {
		if len(us.index.Sessions) > 0 {
			us.index.Current = us.index.Sessions[len(us.index.Sessions)-1].ID
		} else {
			meta := us.createLocked()
			us.index.Current = meta.ID
		}
	}
	m.saveIndex(ctx, us)
	return us.getOrLoadLocked(ctx, m.store, us.index.Current), nil
}

// ClearCurrent wipes the current session's history, skills and pending state
// without removing the session itself.
func (m *SessionManager) ClearCurrent(ctx context.Context, userID string) {
	sess := m.Current(ctx, userID)
	sess.Mu.Lock()
	sess.History = nil
	sess.ActiveSkills = make(map[string]struct{})
	sess.Pending = nil
	sess.Mu.Unlock()
	_ = m.store.Clear(ctx, sess.Key)
}

// UpdateTitle sets a session's title from a message preview, but only while the
// title is still the default (so the first user message names the session).
func (m *SessionManager) UpdateTitle(ctx context.Context, userID, id, title string) {
	title = strings.TrimSpace(strings.ReplaceAll(title, "\n", " "))
	if title == "" {
		return
	}
	if r := []rune(title); len(r) > 20 {
		title = string(r[:20]) + "…"
	}
	us := m.user(ctx, userID)
	us.mu.Lock()
	defer us.mu.Unlock()
	for i := range us.index.Sessions {
		if us.index.Sessions[i].ID == id {
			if us.index.Sessions[i].Title == "" || us.index.Sessions[i].Title == defaultTitle {
				us.index.Sessions[i].Title = title
				m.saveIndex(ctx, us)
			}
			return
		}
	}
}

// --- session propagation through context (for skill-management tools) ---

type ctxKey int

const sessionCtxKey ctxKey = iota

// withSession returns a context carrying the active session.
func withSession(ctx context.Context, sess *Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey, sess)
}

// sessionFromContext extracts the active session, if any.
func sessionFromContext(ctx context.Context) (*Session, bool) {
	sess, ok := ctx.Value(sessionCtxKey).(*Session)
	return sess, ok
}
