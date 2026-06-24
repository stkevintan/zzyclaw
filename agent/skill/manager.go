package skill

import (
	"fmt"
	"sort"
	"sync"
)

// Manager layers a private, per-user skill registry over a shared set of builtin
// skills. Builtin skills are compiled into the binary and served from memory
// (see builtinSkills); they are visible to every user but owned by none — they
// cannot be created, overwritten or deleted by a user. The global registry holds
// any additional operator-provided shared skills on disk. Every other skill
// lives in the calling user's own directory and is invisible to — and unusable
// by — any other user.
//
// All user-scoped methods take a userID. With an empty userID only the builtin
// (and any shared on-disk) skills are visible (used for non-user contexts such
// as tests).
type Manager struct {
	global *Registry

	// userDir resolves the on-disk skills directory for a user. It is supplied by
	// the caller so the skill package need not know the workspace layout; in
	// production it returns <workspace>/<userID>/skills.
	userDir func(userID string) (string, error)

	mu    sync.Mutex
	users map[string]*Registry // userID -> that user's registry (created lazily)
}

// NewManager builds a manager whose builtin skills are compiled in (served from
// memory). globalDir is an optional shared directory for operator-provided
// skills, and per-user skills live in the directory returned by userDir.
func NewManager(globalDir string, userDir func(userID string) (string, error)) (*Manager, error) {
	g, err := NewRegistry(globalDir)
	if err != nil {
		return nil, err
	}
	return &Manager{
		global:  g,
		userDir: userDir,
		users:   make(map[string]*Registry),
	}, nil
}

// userRegistry returns the (lazily created) registry for userID, or nil when
// userID is empty or no per-user resolver is configured. Disk I/O (directory
// creation and the initial scan) happens outside the lock so one user's first
// access never blocks other users; the lock is held only to read and update the
// cache, with a second check to avoid racing duplicate registries.
func (m *Manager) userRegistry(userID string) (*Registry, error) {
	if userID == "" || m.userDir == nil {
		return nil, nil
	}
	m.mu.Lock()
	r, ok := m.users[userID]
	m.mu.Unlock()
	if ok {
		return r, nil
	}
	dir, err := m.userDir(userID)
	if err != nil {
		return nil, fmt.Errorf("skill: resolve user dir: %w", err)
	}
	reg, err := NewRegistry(dir)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok = m.users[userID]; ok {
		return r, nil
	}
	m.users[userID] = reg
	return reg, nil
}

// List returns the builtin skills plus userID's own skills and any shared
// on-disk skills, sorted by name. A disk skill can never shadow a builtin (those
// names are reserved), a user's private skill shadows a shared skill of the same
// name (most-specific layer wins), and duplicate names are reported once.
func (m *Manager) List(userID string) []*Skill {
	out := builtinList()
	seen := make(map[string]bool, len(out))
	for _, s := range out {
		seen[s.Name] = true
	}
	add := func(skills []*Skill) {
		for _, s := range skills {
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			out = append(out, s)
		}
	}
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		add(ur.List())
	}
	add(m.global.List())
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get resolves a skill by name for userID, checking the compiled-in builtins
// first, then the user's own skills, then any shared on-disk skills. A user's
// private skill thus shadows a shared skill of the same name. The builtin is
// returned as a shallow copy so callers can't mutate the shared original, and
// Get never returns another user's skill.
func (m *Manager) Get(userID, name string) (*Skill, bool) {
	if s, ok := builtinSkills[name]; ok {
		sCopy := *s
		return &sCopy, true
	}
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		if s, ok := ur.Get(name); ok && !builtinSkillSet[s.Name] {
			return s, true
		}
	}
	if s, ok := m.global.Get(name); ok && !builtinSkillSet[s.Name] {
		return s, true
	}
	return nil, false
}

// Scope reports where Get would resolve name for userID: "builtin", "private"
// (the user's own skill), "shared" (the on-disk shared registry) or "" when the
// name is unknown. Because a private skill shadows a shared one, Scope returns
// "private" when the user has their own copy even if a shared skill of the same
// name also exists.
func (m *Manager) Scope(userID, name string) string {
	if builtinSkillSet[name] {
		return "builtin"
	}
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		if _, ok := ur.Get(name); ok {
			return "private"
		}
	}
	if _, ok := m.global.Get(name); ok {
		return "shared"
	}
	return ""
}

// Create writes (or updates) a skill in userID's own directory. Builtin skills
// cannot be overwritten. A user context is required.
func (m *Manager) Create(userID, name, skillMD, entryFile, entryCode string) error {
	ur, err := m.userRegistry(userID)
	if err != nil {
		return err
	}
	if ur == nil {
		return fmt.Errorf("skills can only be created for a specific user")
	}
	return ur.Create(name, skillMD, entryFile, entryCode)
}

// Remove deletes a skill from userID's own directory. Builtin skills are
// protected and a user can never delete another user's skill.
func (m *Manager) Remove(userID, name string) error {
	ur, err := m.userRegistry(userID)
	if err != nil {
		return err
	}
	if ur == nil {
		return fmt.Errorf("skills can only be deleted for a specific user")
	}
	return ur.Remove(name)
}

// CreateShared writes (or updates) a skill in the shared on-disk registry, making
// it visible to and runnable by every user. Builtin skills cannot be overwritten.
// Authorization (e.g. owner-only) is enforced by the caller, since a shared skill
// affects all users.
func (m *Manager) CreateShared(name, skillMD, entryFile, entryCode string) error {
	return m.global.Create(name, skillMD, entryFile, entryCode)
}

// RemoveShared deletes a skill from the shared on-disk registry. Builtin skills
// are protected. Authorization is enforced by the caller.
func (m *Manager) RemoveShared(name string) error {
	return m.global.Remove(name)
}

// Reload rescans skills from disk so changes made directly on disk (e.g. a
// SKILL.md edited or a skill folder dropped in) are picked up. It always
// rescans the shared on-disk registry and, for a non-empty userID, that user's
// own skills too. Builtins are compiled in and need no reload.
func (m *Manager) Reload(userID string) error {
	if err := m.global.Reload(); err != nil {
		return err
	}
	ur, err := m.userRegistry(userID)
	if err != nil {
		return err
	}
	if ur != nil {
		return ur.Reload()
	}
	return nil
}
