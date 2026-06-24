package skill

import (
	"fmt"
	"sort"
	"sync"
)

// Manager layers a private, per-user skill registry over a shared set of builtin
// skills. Builtin skills (seeded into the global directory) are visible to every
// user but owned by none: they cannot be created, overwritten or deleted by a
// user. Every other skill lives in the calling user's own directory and is
// invisible to — and unusable by — any other user.
//
// All user-scoped methods take a userID. With an empty userID only the global
// builtin skills are visible (used for non-user contexts such as tests).
type Manager struct {
	global *Registry

	// userDir resolves the on-disk skills directory for a user. It is supplied by
	// the caller so the skill package need not know the workspace layout; in
	// production it returns <workspace>/<userID>/skills.
	userDir func(userID string) (string, error)

	mu    sync.Mutex
	users map[string]*Registry // userID -> that user's registry (created lazily)
}

// NewManager builds a manager whose builtin skills live in globalDir (seeded on
// creation) and whose per-user skills live in the directory returned by userDir.
func NewManager(globalDir string, userDir func(userID string) (string, error)) (*Manager, error) {
	g, err := NewRegistry(globalDir)
	if err != nil {
		return nil, err
	}
	if err := g.Seed(); err != nil {
		return nil, err
	}
	return &Manager{
		global:  g,
		userDir: userDir,
		users:   make(map[string]*Registry),
	}, nil
}

// userRegistry returns the (lazily created) registry for userID, or nil when
// userID is empty or no per-user resolver is configured.
func (m *Manager) userRegistry(userID string) (*Registry, error) {
	if userID == "" || m.userDir == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.users[userID]; ok {
		return r, nil
	}
	dir, err := m.userDir(userID)
	if err != nil {
		return nil, fmt.Errorf("skill: resolve user dir: %w", err)
	}
	r, err := NewRegistry(dir)
	if err != nil {
		return nil, err
	}
	m.users[userID] = r
	return r, nil
}

// List returns the builtin skills plus userID's own skills, sorted by name. A
// user skill can never shadow a builtin (those names are reserved).
func (m *Manager) List(userID string) []*Skill {
	out := m.global.List()
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		for _, s := range ur.List() {
			if builtinSkills[s.Name] {
				continue
			}
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get resolves a skill by name for userID, checking the shared builtins first
// and then the user's own skills. It never returns another user's skill.
func (m *Manager) Get(userID, name string) (*Skill, bool) {
	if s, ok := m.global.Get(name); ok {
		return s, true
	}
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		if s, ok := ur.Get(name); ok && !builtinSkills[s.Name] {
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

// Reload rescans the shared builtins and userID's own skills from disk.
func (m *Manager) Reload(userID string) error {
	if err := m.global.Reload(); err != nil {
		return err
	}
	if ur, err := m.userRegistry(userID); err == nil && ur != nil {
		return ur.Reload()
	}
	return nil
}
