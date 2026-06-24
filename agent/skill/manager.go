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

// List returns the builtin skills plus any shared on-disk skills and userID's
// own skills, sorted by name. A disk skill can never shadow a builtin (those
// names are reserved), and duplicate names are reported once.
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
	add(m.global.List())
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		add(ur.List())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get resolves a skill by name for userID, checking the compiled-in builtins
// first, then any shared on-disk skills, then the user's own skills. It never
// returns another user's skill.
func (m *Manager) Get(userID, name string) (*Skill, bool) {
	if s, ok := builtinSkills[name]; ok {
		return s, true
	}
	if s, ok := m.global.Get(name); ok && !builtinSkillSet[s.Name] {
		return s, true
	}
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		if s, ok := ur.Get(name); ok && !builtinSkillSet[s.Name] {
			return s, true
		}
	}
	return nil, false
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

// Reload rescans skills from disk. With an empty userID it rescans the shared
// builtins; otherwise it rescans only userID's own skills (the builtins are
// static and seeded at startup, so reloading them per user would be redundant
// disk I/O).
func (m *Manager) Reload(userID string) error {
	if userID == "" {
		return m.global.Reload()
	}
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		return ur.Reload()
	}
	return nil
}
