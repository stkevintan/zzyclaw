package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// maxShellOutput caps the combined output returned to the model so a verbose
// build or test run cannot blow up the conversation context. The tail is kept
// because compiler errors and test summaries usually appear at the end.
const maxShellOutput = 16000

// toolNameRunShell is the function name exposed to the model for the shell tool.
const toolNameRunShell = "run_shell"

// shellTool executes an arbitrary shell command inside the agent workspace. It
// is the primary tool for a coding agent: compiling, running and testing code,
// using git, inspecting the tree, etc. Path confinement is best-effort (the
// working directory is restricted to the sandbox); real isolation comes from
// running the agent inside its container plus the approval flow.
type shellTool struct {
	sb      *Sandbox
	timeout time.Duration
}

// NewShell returns a tool that runs an arbitrary command via "sh -c" with the
// working directory confined to the sandbox.
func NewShell(sb *Sandbox, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &shellTool{sb: sb, timeout: timeout}
}

func (t *shellTool) Name() string { return toolNameRunShell }
func (t *shellTool) Description() string {
	return "Execute a shell command (via 'sh -c') inside the agent workspace and return its combined stdout/stderr and exit code. Use this to build, run and test code: compile programs, run test suites, run linters, use git, create directories, etc. Supports pipes, redirection and '&&'. The working directory defaults to the workspace root; pass 'cwd' (relative to the workspace) to run elsewhere. A non-zero exit code is returned as normal output so you can read failures and fix them."
}
func (t *shellTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Shell command to execute, e.g. \"go test ./...\"."},"cwd":{"type":"string","description":"Optional working directory relative to the workspace root."}},"required":["command"]}`)
}

// Dangerous returns false only for simple read-only inspection commands, which
// run without an approval prompt. Everything else (and any command we cannot
// parse) requires approval.
func (t *shellTool) Dangerous(args json.RawMessage) bool {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return true
	}
	return !IsReadOnlyCommand(a.Command)
}

func (t *shellTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", fmt.Errorf("command must not be empty")
	}
	if err := ScanDangerous(a.Command); err != nil {
		return "", err
	}

	dir, err := t.sb.workspaceDir(ctx)
	if err != nil {
		return "", err
	}
	if a.Cwd != "" {
		resolved, err := t.sb.resolveCtx(ctx, a.Cwd)
		if err != nil {
			return "", err
		}
		dir = resolved
	}

	cctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", a.Command)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	out := truncateOutput(strings.TrimSpace(buf.String()))

	if cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("command timed out after %s", t.timeout)
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		// Surface the failing command's output and exit code as a normal result
		// (not an error) so the model can read the failure and react.
		if out == "" {
			out = "(no output)"
		}
		return fmt.Sprintf("%s\n\n[exit code %d]", out, exitErr.ExitCode()), nil
	}
	if runErr != nil {
		return out, fmt.Errorf("failed to run command: %w", runErr)
	}
	if out == "" {
		return "(command produced no output, exit code 0)", nil
	}
	return out, nil
}

// truncateOutput keeps the tail of s when it exceeds maxShellOutput, prefixing a
// notice so the model knows output was clipped.
func truncateOutput(s string) string {
	if len(s) <= maxShellOutput {
		return s
	}
	clipped := s[len(s)-maxShellOutput:]
	return fmt.Sprintf("[output truncated to last %d bytes]\n%s", maxShellOutput, clipped)
}
