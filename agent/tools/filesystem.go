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

// resolve validates and returns the absolute path for a user-supplied path.
func (s *Sandbox) resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(s.roots[0], p)
	}
	abs = filepath.Clean(abs)
	for _, root := range s.roots {
		if abs == root || strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed workspace", p)
}

// Roots returns the configured root directories (for prompt context).
func (s *Sandbox) Roots() []string { return append([]string(nil), s.roots...) }

// --- read_file ---

type readFileTool struct{ sb *Sandbox }

// NewReadFile returns a tool that reads a UTF-8 text file inside the sandbox.
func NewReadFile(sb *Sandbox) Tool { return &readFileTool{sb: sb} }

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return "Read the full text contents of a file within the agent workspace."
}
func (t *readFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file, relative to the workspace root."}},"required":["path"]}`)
}
func (t *readFileTool) Dangerous(json.RawMessage) bool { return false }
func (t *readFileTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	abs, err := t.sb.resolve(a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	return string(data), nil
}

// --- write_file ---

type writeFileTool struct{ sb *Sandbox }

// NewWriteFile returns a tool that creates or overwrites a file in the sandbox.
func NewWriteFile(sb *Sandbox) Tool { return &writeFileTool{sb: sb} }

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Create or overwrite a file within the agent workspace, creating parent directories as needed."
}
func (t *writeFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Path to the file, relative to the workspace root."},"content":{"type":"string","description":"Full text content to write."}},"required":["path","content"]}`)
}
func (t *writeFileTool) Dangerous(json.RawMessage) bool { return true }
func (t *writeFileTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	abs, err := t.sb.resolve(a.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("create directories: %w", err)
	}
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(a.Content), a.Path), nil
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
func (t *listDirTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Path == "" {
		a.Path = "."
	}
	abs, err := t.sb.resolve(a.Path)
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
func (t *deletePathTool) Dangerous(json.RawMessage) bool { return true }
func (t *deletePathTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	abs, err := t.sb.resolve(a.Path)
	if err != nil {
		return "", err
	}
	for _, root := range t.sb.roots {
		if abs == root {
			return "", fmt.Errorf("refusing to delete the workspace root")
		}
	}
	if err := os.RemoveAll(abs); err != nil {
		return "", fmt.Errorf("delete: %w", err)
	}
	return "Deleted " + a.Path, nil
}
