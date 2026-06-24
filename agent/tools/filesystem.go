package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sandbox restricts filesystem access to a set of allowed root directories.
// Any path that resolves outside every root (including via "..") is rejected.
type Sandbox struct {
	roots []string
}

// NewSandbox builds a sandbox over the given root directories. Roots are cleaned
// and made absolute. The first root is used as the base for relative paths.
func NewSandbox(roots ...string) (*Sandbox, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("sandbox: at least one root is required")
	}
	abs := make([]string, 0, len(roots))
	for _, r := range roots {
		a, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("sandbox: resolve root %q: %w", r, err)
		}
		abs = append(abs, filepath.Clean(a))
	}
	return &Sandbox{roots: abs}, nil
}

// userCtxKey carries the active user ID so the sandbox can confine the workspace
// to a per-user subdirectory.
type userCtxKey struct{}

// WithUser returns a context carrying userID. The filesystem, search and shell
// tools use it to confine relative paths (and the shell working directory) to
// the user's own workspace subdirectory, keeping each user's files separate.
func WithUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userCtxKey{}, userID)
}

func userFromContext(ctx context.Context) string {
	id, _ := ctx.Value(userCtxKey{}).(string)
	return id
}

// sanitizeUser maps a user ID to a single safe path segment, preventing path
// traversal. WeChat user IDs are alphanumeric/underscore, so this is normally a
// no-op; any other character collapses to '_'.
func sanitizeUser(userID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, userID)
	if safe == "" || safe == "." || safe == ".." {
		return "_"
	}
	return safe
}

// userWorkspace returns the workspace base for userID: a per-user subdirectory
// of the primary root. With an empty userID it returns the primary root itself
// (used by the approval/grant checks and in non-user contexts such as tests).
func (s *Sandbox) userWorkspace(userID string) string {
	if userID == "" {
		return s.roots[0]
	}
	return filepath.Join(s.roots[0], sanitizeUser(userID))
}

// UserWorkspace returns the per-user subdirectory under base (created if
// necessary), mirroring the sandbox's per-user layout so skills and the
// filesystem tools share the same directory for a given user. With an empty
// base or userID it returns base unchanged.
func UserWorkspace(base, userID string) (string, error) {
	if base == "" || userID == "" {
		return base, nil
	}
	dir := filepath.Join(base, sanitizeUser(userID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// resolveFor validates p against the roots available to userID and returns its
// absolute path. Relative paths are taken against the user's own workspace; the
// user may reach their workspace subtree plus any shared root (e.g. the skills
// directory) but never another user's workspace.
func (s *Sandbox) resolveFor(userID, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	base := s.userWorkspace(userID)
	if userID != "" {
		if err := os.MkdirAll(base, 0o755); err != nil {
			return "", fmt.Errorf("create user workspace: %w", err)
		}
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(base, p)
	}
	abs = filepath.Clean(abs)
	allowed := append([]string{base}, s.roots[1:]...)
	for _, root := range allowed {
		if abs == root || strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed workspace", p)
}

// resolve validates a path without a user scope: relative paths resolve against
// the shared primary root and every root is reachable. It backs the
// approval/grant checks, which run before the per-user scope is applied; the
// actual reads and writes use resolveCtx so they land in the per-user workspace.
func (s *Sandbox) resolve(p string) (string, error) {
	return s.resolveFor("", p)
}

// resolveCtx resolves p within the workspace of the user carried by ctx.
func (s *Sandbox) resolveCtx(ctx context.Context, p string) (string, error) {
	return s.resolveFor(userFromContext(ctx), p)
}

// workspaceDir returns the (existing) per-user workspace directory for ctx.
func (s *Sandbox) workspaceDir(ctx context.Context) (string, error) {
	return s.resolveFor(userFromContext(ctx), ".")
}

// isProtectedRoot reports whether abs is a directory that must never be deleted:
// any configured sandbox root or the caller's own workspace directory.
func (s *Sandbox) isProtectedRoot(ctx context.Context, abs string) bool {
	if uw, err := s.workspaceDir(ctx); err == nil && abs == uw {
		return true
	}
	for _, root := range s.roots {
		if abs == root {
			return true
		}
	}
	return false
}

// Roots returns the configured root directories (for prompt context).
func (s *Sandbox) Roots() []string { return append([]string(nil), s.roots...) }

// grantScopeForPath builds a directory-scoped grant key+label for a filesystem
// mutation of path. The scope is the containing directory, so approving one edit
// with "always" lets the agent keep modifying files in that same directory
// without re-prompting. All mutating tools share the "fs_write" prefix so the
// grant covers writes, edits and deletes alike. ok=false when the path can't be
// resolved inside the sandbox (the call must then be approved every time).
//
// When path is itself an existing directory (e.g. deleting a sub-directory) the
// scope is that directory, not its parent: otherwise approving the deletion of
// one sub-directory would silently grant write/delete over the whole parent
// (potentially the workspace root) — a privilege escalation.
func grantScopeForPath(sb *Sandbox, rawPath string) (key, label string, ok bool) {
	if rawPath == "" {
		return "", "", false
	}
	abs, err := sb.resolve(rawPath)
	if err != nil {
		return "", "", false
	}
	dir := abs
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		dir = filepath.Dir(abs)
	}
	return "fs_write:" + dir, "file changes under " + dir, true
}

// inWorkspace reports whether abs lies within the primary workspace root
// (roots[0]). The workspace is the agent's scratch area and is writable without
// approval; other roots (e.g. the skills directory) stay approval-gated.
func (s *Sandbox) inWorkspace(abs string) bool {
	root := s.roots[0]
	return abs == root || strings.HasPrefix(abs, root+string(os.PathSeparator))
}

// mutationNeedsApproval reports whether a filesystem mutation described by args
// (which carry a "path" field) requires approval. Mutations inside the workspace
// root are pre-approved by default; anything else (the skills directory, or an
// unresolved/out-of-sandbox path) still goes through the approval gate.
func mutationNeedsApproval(sb *Sandbox, args json.RawMessage) bool {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.Path == "" {
		return true
	}
	abs, err := sb.resolve(a.Path)
	if err != nil {
		return true
	}
	return !sb.inWorkspace(abs)
}

// --- read_file ---

type readFileTool struct{ sb *Sandbox }

// NewReadFile returns a tool that reads a UTF-8 text file inside the sandbox.
func NewReadFile(sb *Sandbox) Tool { return &readFileTool{sb: sb} }

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return "Read the text contents of a file within the agent workspace. Optionally read only an inclusive 1-based line range via start_line/end_line."
}
func (t *readFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file, relative to the workspace root."},"start_line":{"type":"integer","description":"Optional 1-based first line to read. Omit to start at the top."},"end_line":{"type":"integer","description":"Optional 1-based last line to read (inclusive). Omit to read to the end."}},"required":["path"]}`)
}
func (t *readFileTool) Dangerous(json.RawMessage) bool { return false }
func (t *readFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	abs, err := t.sb.resolveCtx(ctx, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	if a.StartLine == 0 && a.EndLine == 0 {
		return string(data), nil
	}
	lines := strings.Split(string(data), "\n")
	start := a.StartLine
	if start < 1 {
		start = 1
	}
	if start > len(lines) {
		return "", fmt.Errorf("start_line %d is past the end of the file (%d lines)", a.StartLine, len(lines))
	}
	end := a.EndLine
	if end == 0 || end > len(lines) {
		end = len(lines)
	}
	if end < start {
		return "", fmt.Errorf("end_line %d is before start_line %d", a.EndLine, start)
	}
	return strings.Join(lines[start-1:end], "\n"), nil
}

// --- write_file ---

type writeFileTool struct{ sb *Sandbox }

// NewWriteFile returns a tool that creates or overwrites a file in the sandbox.
func NewWriteFile(sb *Sandbox) Tool { return &writeFileTool{sb: sb} }

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Create or overwrite a file within the agent workspace, creating parent directories as needed. Set append to add to the end of an existing file instead of overwriting it."
}
func (t *writeFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file, relative to the workspace root."},"content":{"type":"string","description":"Text content to write (or append)."},"append":{"type":"boolean","description":"If true, append to the file instead of overwriting it."}},"required":["path","content"]}`)
}

// Dangerous gates writes outside the workspace (e.g. the skills directory);
// writes within the workspace root are pre-approved.
func (t *writeFileTool) Dangerous(args json.RawMessage) bool {
	return mutationNeedsApproval(t.sb, args)
}

// GrantScope remembers approval per target directory (see grantScopeForPath).
func (t *writeFileTool) GrantScope(args json.RawMessage) (key, label string, ok bool) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", "", false
	}
	return grantScopeForPath(t.sb, a.Path)
}
func (t *writeFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	abs, err := t.sb.resolveCtx(ctx, a.Path)
	if err != nil {
		return "", err
	}
	// Refuse to create executable scripts that contain denylisted patterns, so
	// the skill writer cannot scaffold a dangerous script. When appending, scan
	// the resulting file so a payload cannot be split across multiple appends.
	if isScriptPath(abs) {
		toScan := a.Content
		if a.Append {
			if existing, rerr := os.ReadFile(abs); rerr == nil {
				toScan = string(existing) + a.Content
			}
		}
		if err := ScanDangerous(toScan); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("create directories: %w", err)
	}
	if a.Append {
		f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return "", fmt.Errorf("open file: %w", err)
		}
		if _, err := f.WriteString(a.Content); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("append file: %w", err)
		}
		if err := f.Close(); err != nil {
			return "", fmt.Errorf("close file: %w", err)
		}
		return fmt.Sprintf("Appended %d bytes to %s", len(a.Content), a.Path), nil
	}
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), a.Path), nil
}

// --- edit_file ---

type editFileTool struct{ sb *Sandbox }

// NewEditFile returns a tool that replaces an exact text snippet inside an
// existing file. It is the structured, deterministic alternative to `sed -i`.
func NewEditFile(sb *Sandbox) Tool { return &editFileTool{sb: sb} }

func (t *editFileTool) Name() string { return "edit_file" }
func (t *editFileTool) Description() string {
	return "Replace an exact text snippet within an existing file in the agent workspace. By default old_string must match exactly one location; set replace_all to replace every occurrence. Prefer this over shell sed/awk for editing files."
}
func (t *editFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file, relative to the workspace root."},"old_string":{"type":"string","description":"Exact text to find. Include enough surrounding context to match a single location."},"new_string":{"type":"string","description":"Replacement text."},"replace_all":{"type":"boolean","description":"Replace every occurrence instead of requiring exactly one match."}},"required":["path","old_string","new_string"]}`)
}

// Dangerous gates edits outside the workspace (e.g. the skills directory);
// edits within the workspace root are pre-approved.
func (t *editFileTool) Dangerous(args json.RawMessage) bool {
	return mutationNeedsApproval(t.sb, args)
}

// GrantScope remembers approval per target directory (see grantScopeForPath).
func (t *editFileTool) GrantScope(args json.RawMessage) (key, label string, ok bool) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", "", false
	}
	return grantScopeForPath(t.sb, a.Path)
}
func (t *editFileTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.OldString == "" {
		return "", fmt.Errorf("old_string must not be empty")
	}
	if a.OldString == a.NewString {
		return "", fmt.Errorf("old_string and new_string are identical")
	}
	abs, err := t.sb.resolveCtx(ctx, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	content := string(data)
	count := strings.Count(content, a.OldString)
	if count == 0 {
		return "", fmt.Errorf("old_string not found in %s", a.Path)
	}
	if count > 1 && !a.ReplaceAll {
		return "", fmt.Errorf("old_string matches %d locations in %s; add more context to make it unique or set replace_all", count, a.Path)
	}
	var updated string
	if a.ReplaceAll {
		updated = strings.ReplaceAll(content, a.OldString, a.NewString)
	} else {
		updated = strings.Replace(content, a.OldString, a.NewString, 1)
	}
	// Keep the script denylist enforced for in-place edits too.
	if isScriptPath(abs) {
		if err := ScanDangerous(updated); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	replaced := 1
	if a.ReplaceAll {
		replaced = count
	}
	return fmt.Sprintf("Replaced %d occurrence(s) in %s", replaced, a.Path), nil
}

// --- list_dir ---

type listDirTool struct{ sb *Sandbox }

// NewListDir returns a tool that lists the entries of a directory in the sandbox.
func NewListDir(sb *Sandbox) Tool { return &listDirTool{sb: sb} }

func (t *listDirTool) Name() string { return "list_dir" }
func (t *listDirTool) Description() string {
	return "List the files and subdirectories of a directory within the agent workspace."
}
func (t *listDirTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory path relative to the workspace root. Use \".\" for the root."}},"required":["path"]}`)
}
func (t *listDirTool) Dangerous(json.RawMessage) bool { return false }
func (t *listDirTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Path == "" {
		a.Path = "."
	}
	abs, err := t.sb.resolveCtx(ctx, a.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("list dir: %w", err)
	}
	if len(entries) == 0 {
		return "(empty directory)", nil
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n"), nil
}

// --- delete_path ---

type deletePathTool struct{ sb *Sandbox }

// NewDeletePath returns a tool that deletes a file or directory in the sandbox.
func NewDeletePath(sb *Sandbox) Tool { return &deletePathTool{sb: sb} }

func (t *deletePathTool) Name() string { return "delete_path" }
func (t *deletePathTool) Description() string {
	return "Delete a file or directory (recursively) within the agent workspace."
}
func (t *deletePathTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path to delete, relative to the workspace root."}},"required":["path"]}`)
}

// Dangerous gates deletions outside the workspace (e.g. the skills directory);
// deletions within the workspace root are pre-approved.
func (t *deletePathTool) Dangerous(args json.RawMessage) bool {
	return mutationNeedsApproval(t.sb, args)
}

// GrantScope remembers approval per target directory (see grantScopeForPath).
func (t *deletePathTool) GrantScope(args json.RawMessage) (key, label string, ok bool) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", "", false
	}
	return grantScopeForPath(t.sb, a.Path)
}
func (t *deletePathTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	abs, err := t.sb.resolveCtx(ctx, a.Path)
	if err != nil {
		return "", err
	}
	if t.sb.isProtectedRoot(ctx, abs) {
		return "", fmt.Errorf("refusing to delete the workspace root")
	}
	if err := os.RemoveAll(abs); err != nil {
		return "", fmt.Errorf("delete: %w", err)
	}
	return "Deleted " + a.Path, nil
}
