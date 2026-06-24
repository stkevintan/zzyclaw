package tools

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// DenoRunner executes a skill's JavaScript/TypeScript entry file inside the Deno
// sandbox. Deno is deny-by-default: the guest gets ONLY the read/write paths and
// network hosts explicitly granted per run, and nothing else (no env, no
// subprocess spawning, no FFI, no remote module loading). It is the single
// execution tier for user-added skills.
type DenoRunner struct {
	bin           string        // deno binary (absolute path or a name resolved via PATH)
	cacheDir      string        // DENO_DIR for Deno's internal cache (kept off the skill/workspace)
	timeout       time.Duration // wall-clock budget per run
	maxOldSpaceMB int           // V8 --max-old-space-size cap in MB; <=0 leaves Deno's default
}

// NewDenoRunner returns a runner backed by the Deno binary at denoPath (or
// "deno" on PATH when empty). cacheDir is used as DENO_DIR so Deno's internal
// cache never touches the skill or workspace directories. maxOldSpaceMB caps the
// V8 heap (--max-old-space-size) so a runaway allocation OOMs the contained
// process instead of pressuring the host; <=0 leaves Deno's default.
func NewDenoRunner(denoPath, cacheDir string, timeout time.Duration, maxOldSpaceMB int) *DenoRunner {
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
	return &DenoRunner{bin: denoPath, cacheDir: cacheDir, timeout: timeout, maxOldSpaceMB: maxOldSpaceMB}
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
	Env   []string // environment variable names the run may read (Deno --allow-env)
}

// denoArgs builds the deno argv for an entry file with the given permissions.
// It is pure so it can be unit-tested without invoking Deno.
func denoArgs(entryPath string, scriptArgs []string, perms DenoPermissions, maxOldSpaceMB int) []string {
	// Deny-by-default flags: no prompting (fail closed), no remote module
	// fetching (a skill cannot pull arbitrary code at import time), and no
	// implicit config/lockfile discovery from the skill directory.
	argv := []string{"run", "--no-prompt", "--no-remote", "--no-config", "--quiet"}
	// Cap the V8 heap so a runaway allocation crashes the sandboxed process with
	// an OOM (contained) instead of consuming host memory up to the timeout.
	if maxOldSpaceMB > 0 {
		argv = append(argv, fmt.Sprintf("--v8-flags=--max-old-space-size=%d", maxOldSpaceMB))
	}
	if len(perms.Read) > 0 {
		argv = append(argv, "--allow-read="+strings.Join(perms.Read, ","))
	}
	if len(perms.Write) > 0 {
		argv = append(argv, "--allow-write="+strings.Join(perms.Write, ","))
	}
	if len(perms.Net) > 0 {
		// A "*" entry means "any host": Deno grants all network access when
		// --allow-net is passed without a value. Otherwise restrict to the
		// listed hosts.
		if slices.Contains(perms.Net, "*") {
			argv = append(argv, "--allow-net")
		} else {
			argv = append(argv, "--allow-net="+strings.Join(perms.Net, ","))
		}
	}
	if len(perms.Env) > 0 {
		// Always scope env access to the declared names (never bare --allow-env):
		// the host environment is otherwise hidden from skills, so an unscoped
		// grant would be a needless secret-exposure risk.
		argv = append(argv, "--allow-env="+strings.Join(perms.Env, ","))
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

	cmd := exec.CommandContext(cctx, r.bin, denoArgs(entryPath, scriptArgs, perms, r.maxOldSpaceMB)...)
	cmd.Dir = filepath.Dir(entryPath)
	// Minimal, fixed environment. The host environment is hidden from skills by
	// default; only the variable names a skill explicitly declares (and that exist
	// on the host) are passed through, paired with --allow-env for those names.
	// The sandbox-defining variables are written LAST so a skill can never
	// override them (e.g. redirect DENO_DIR to a directory it controls).
	env := make(map[string]string, len(perms.Env)+3)
	for _, name := range perms.Env {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	env["DENO_DIR"] = r.cacheDir
	env["DENO_NO_UPDATE_CHECK"] = "1"
	env["NO_COLOR"] = "1"
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	// When log.level=debug, dump the exact command we hand to Deno so module
	// resolution / permission failures can be diagnosed.
	slog.Debug("deno skill: exec",
		"argv", cmd.Args,
		"dir", cmd.Dir,
		"deno_dir", r.cacheDir,
		"read", perms.Read,
		"write", perms.Write,
		"net", perms.Net,
		"env", perms.Env,
	)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	raw := strings.TrimSpace(buf.String())
	out := truncateOutput(raw)

	// Dump the full, untruncated stdout+stderr (and exit status) at debug level.
	slog.Debug("deno skill: result",
		"entry", entryPath,
		"err", err,
		"output", raw,
	)

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
