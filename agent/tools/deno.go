package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DenoRunner executes a skill's JavaScript/TypeScript entry file inside the Deno
// sandbox. Deno is deny-by-default: the guest gets ONLY the read/write paths and
// network hosts explicitly granted per run, and nothing else (no env, no
// subprocess spawning, no FFI, no remote module loading). It is the single
// execution tier for user-added skills.
type DenoRunner struct {
	bin      string        // deno binary (absolute path or a name resolved via PATH)
	cacheDir string        // DENO_DIR for Deno's internal cache (kept off the skill/workspace)
	timeout  time.Duration // wall-clock budget per run
}

// NewDenoRunner returns a runner backed by the Deno binary at denoPath (or
// "deno" on PATH when empty). cacheDir is used as DENO_DIR so Deno's internal
// cache never touches the skill or workspace directories.
func NewDenoRunner(denoPath, cacheDir string, timeout time.Duration) *DenoRunner {
	if denoPath == "" {
		denoPath = "deno"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// DENO_DIR is passed to Deno whose working directory is the skill dir; make it
	// absolute so the cache never gets created inside the skill directory.
	if abs, err := filepath.Abs(cacheDir); err == nil {
		cacheDir = abs
	}
	return &DenoRunner{bin: denoPath, cacheDir: cacheDir, timeout: timeout}
}

// Installed reports whether the Deno binary can be located.
func (r *DenoRunner) Installed() bool {
	_, err := exec.LookPath(r.bin)
	return err == nil
}

// Path returns the configured Deno binary (for diagnostics).
func (r *DenoRunner) Path() string { return r.bin }

// DenoPermissions is the capability grant for a single run. Anything not listed
// here is denied by Deno.
type DenoPermissions struct {
	Read  []string // absolute paths granted read access
	Write []string // absolute paths granted write access
	Net   []string // network hosts (host or host:port) the run may reach
}

// denoArgs builds the deno argv for an entry file with the given permissions.
// It is pure so it can be unit-tested without invoking Deno.
func denoArgs(entryPath string, scriptArgs []string, perms DenoPermissions) []string {
	// Deny-by-default flags: no prompting (fail closed), no remote module
	// fetching (a skill cannot pull arbitrary code at import time), and no
	// implicit config/lockfile discovery from the skill directory.
	argv := []string{"run", "--no-prompt", "--no-remote", "--no-config", "--quiet"}
	if len(perms.Read) > 0 {
		argv = append(argv, "--allow-read="+strings.Join(perms.Read, ","))
	}
	if len(perms.Write) > 0 {
		argv = append(argv, "--allow-write="+strings.Join(perms.Write, ","))
	}
	if len(perms.Net) > 0 {
		argv = append(argv, "--allow-net="+strings.Join(perms.Net, ","))
	}
	argv = append(argv, entryPath)
	argv = append(argv, scriptArgs...)
	return argv
}

// Run executes entryPath (an absolute path to a .js/.ts file) with the given
// permissions and script arguments, returning combined stdout/stderr.
func (r *DenoRunner) Run(ctx context.Context, entryPath string, scriptArgs []string, perms DenoPermissions) (string, error) {
	if !r.Installed() {
		return "", fmt.Errorf("deno is not installed; install Deno or set agent.deno_path")
	}
	if r.cacheDir != "" {
		if err := os.MkdirAll(r.cacheDir, 0o755); err != nil {
			return "", fmt.Errorf("create deno cache dir: %w", err)
		}
	}

	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, r.bin, denoArgs(entryPath, scriptArgs, perms)...)
	cmd.Dir = filepath.Dir(entryPath)
	// Minimal, fixed environment. Deno reads no env from user code (no
	// --allow-env), and we keep its cache off the mounted data paths.
	cmd.Env = []string{
		"DENO_DIR=" + r.cacheDir,
		"DENO_NO_UPDATE_CHECK=1",
		"NO_COLOR=1",
	}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := truncateOutput(strings.TrimSpace(buf.String()))

	if cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("sandboxed skill timed out after %s", r.timeout)
	}
	if err != nil {
		// Surface the skill's own output alongside a non-zero exit so the model
		// can see and react to failures instead of a bare error.
		if exitErr, ok := err.(*exec.ExitError); ok {
			if out == "" {
				out = fmt.Sprintf("(no output, exit code %d)", exitErr.ExitCode())
			} else {
				out = fmt.Sprintf("%s\n\n[exit code %d]", out, exitErr.ExitCode())
			}
			return out, nil
		}
		return out, fmt.Errorf("sandboxed skill failed: %w", err)
	}
	if out == "" {
		out = "(skill produced no output)"
	}
	return out, nil
}
