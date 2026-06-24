package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// searchFilesTool searches file contents for a regular-expression pattern using
// ripgrep (rg), confined to the sandbox. It works for plain text and source code
// and returns matches as "file:line:text".
type searchFilesTool struct {
	sb      *Sandbox
	timeout time.Duration
}

// NewSearchFiles returns a tool that searches files with ripgrep.
func NewSearchFiles(sb *Sandbox, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &searchFilesTool{sb: sb, timeout: timeout}
}

func (t *searchFilesTool) Name() string { return "search_files" }
func (t *searchFilesTool) Description() string {
	return "Search file contents for a regular-expression pattern (via ripgrep) within the agent workspace. Useful for finding text or code. Returns matching lines as file:line:text."
}
func (t *searchFilesTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression to search for."},"path":{"type":"string","description":"File or directory to search, relative to the workspace root. Defaults to the whole workspace."},"glob":{"type":"string","description":"Optional filename glob filter, e.g. \"*.go\" or \"*.py\"."},"ignore_case":{"type":"boolean","description":"Case-insensitive search when true."},"max_results":{"type":"integer","description":"Maximum number of matching lines to return (default 100)."}},"required":["pattern"]}`)
}
func (t *searchFilesTool) Dangerous(json.RawMessage) bool { return false }

func (t *searchFilesTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("pattern must not be empty")
	}
	if a.Path == "" {
		a.Path = "."
	}
	if a.MaxResults <= 0 {
		a.MaxResults = 100
	}

	abs, err := t.sb.resolve(a.Path)
	if err != nil {
		return "", err
	}

	// Run rg from the search root so result paths are relative and tidy.
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("path not found: %s", a.Path)
	}
	var dir, target string
	if info.IsDir() {
		dir, target = abs, "."
	} else {
		dir, target = filepath.Dir(abs), filepath.Base(abs)
	}

	rgArgs := []string{"--line-number", "--no-heading", "--with-filename", "--color=never"}
	if a.IgnoreCase {
		rgArgs = append(rgArgs, "--ignore-case")
	}
	if a.Glob != "" {
		rgArgs = append(rgArgs, "--glob", a.Glob)
	}
	// Use -e so the pattern is never interpreted as a flag.
	rgArgs = append(rgArgs, "-e", a.Pattern, target)

	cctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "rg", rgArgs...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	if cctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("search timed out after %s", t.timeout)
	}

	out := strings.TrimRight(buf.String(), "\n")

	// ripgrep exits 1 when there are no matches; that is not an error.
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if exitErr.ExitCode() == 1 && out == "" {
			return "No matches found.", nil
		}
		if exitErr.ExitCode() >= 2 {
			return "", fmt.Errorf("ripgrep failed: %s", strings.TrimSpace(out))
		}
	} else if runErr != nil {
		return "", fmt.Errorf("ripgrep failed: %w", runErr)
	}

	if out == "" {
		return "No matches found.", nil
	}

	lines := strings.Split(out, "\n")
	truncated := false
	if len(lines) > a.MaxResults {
		lines = lines[:a.MaxResults]
		truncated = true
	}
	result := strings.Join(lines, "\n")
	if truncated {
		result += fmt.Sprintf("\n… (truncated to %d matches)", a.MaxResults)
	}
	return result, nil
}
