package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// runScriptTool executes a CLI script located in the scripts directory. The
// interpreter is chosen from the file extension (.py -> python3, .sh -> sh),
// otherwise the script is executed directly (must have an executable bit/shebang).
type runScriptTool struct {
	scriptsDir string
	timeout    time.Duration
}

// NewRunScript returns a tool that runs a named script from scriptsDir.
func NewRunScript(scriptsDir string, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &runScriptTool{scriptsDir: scriptsDir, timeout: timeout}
}

func (t *runScriptTool) Name() string { return "run_script" }
func (t *runScriptTool) Description() string {
	return "Execute a CLI script stored in the scripts directory and return its combined stdout/stderr. Use list_dir on the scripts directory to discover available scripts."
}
func (t *runScriptTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"script":{"type":"string","description":"Filename of the script inside the scripts directory."},"args":{"type":"array","items":{"type":"string"},"description":"Command-line arguments to pass to the script."}},"required":["script"]}`)
}
func (t *runScriptTool) Dangerous(json.RawMessage) bool { return true }

func (t *runScriptTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Script string   `json:"script"`
		Args   []string `json:"args"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Script == "" {
		return "", fmt.Errorf("script name must not be empty")
	}
	// Confine to the scripts directory: reject path separators and "..".
	if strings.ContainsAny(a.Script, `/\`) || a.Script == ".." {
		return "", fmt.Errorf("script must be a bare filename inside the scripts directory")
	}
	abs := filepath.Join(t.scriptsDir, a.Script)
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("script not found: %s", a.Script)
	}

	cctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	var cmd *exec.Cmd
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".py":
		cmd = exec.CommandContext(cctx, "python3", append([]string{abs}, a.Args...)...)
	case ".sh":
		cmd = exec.CommandContext(cctx, "sh", append([]string{abs}, a.Args...)...)
	default:
		cmd = exec.CommandContext(cctx, abs, a.Args...)
	}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("script timed out after %s", t.timeout)
	}
	if err != nil {
		return out, fmt.Errorf("script exited with error: %w", err)
	}
	if out == "" {
		out = "(script produced no output)"
	}
	return out, nil
}

// ListScripts returns the available script filenames in scriptsDir.
func ListScripts(scriptsDir string) ([]string, error) {
	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// pipPackageRe validates pip package specifiers (name, optional extras and
// version constraints) so only safe tokens are passed to pip. Arguments are
// passed to exec directly (no shell), so this is defense in depth.
var pipPackageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(\[[A-Za-z0-9,._-]+\])?([=<>!~]=?[A-Za-z0-9._*+-]+)*$`)

// pipInstallTool installs Python packages so that scripts executed by run_script
// can import them.
type pipInstallTool struct {
	timeout time.Duration
}

// NewPipInstall returns a tool that installs Python packages via pip.
func NewPipInstall(timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	return &pipInstallTool{timeout: timeout}
}

func (t *pipInstallTool) Name() string { return "pip_install" }
func (t *pipInstallTool) Description() string {
	return "Install one or more Python packages via pip so that scripts run with run_script can import them. Use this before running a Python script that needs third-party dependencies."
}
func (t *pipInstallTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"packages":{"type":"array","items":{"type":"string"},"description":"Pip package specifiers, e.g. [\"requests\", \"numpy==1.26.4\"]."}},"required":["packages"]}`)
}
func (t *pipInstallTool) Dangerous(json.RawMessage) bool { return true }

func (t *pipInstallTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if len(a.Packages) == 0 {
		return "", fmt.Errorf("at least one package is required")
	}
	pkgs := make([]string, 0, len(a.Packages))
	for _, p := range a.Packages {
		p = strings.TrimSpace(p)
		if !pipPackageRe.MatchString(p) {
			return "", fmt.Errorf("invalid package specifier: %q", p)
		}
		pkgs = append(pkgs, p)
	}

	cctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	// --break-system-packages allows installing into the system interpreter on
	// externally-managed environments (e.g. the Alpine runtime image).
	cmdArgs := append([]string{"-m", "pip", "install", "--no-cache-dir", "--break-system-packages"}, pkgs...)
	cmd := exec.CommandContext(cctx, "python3", cmdArgs...)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.String())
	if cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("pip install timed out after %s", t.timeout)
	}
	if err != nil {
		return out, fmt.Errorf("pip install failed: %w", err)
	}
	if out == "" {
		out = "Installed: " + strings.Join(pkgs, ", ")
	}
	return out, nil
}
