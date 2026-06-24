package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// This file centralizes the agent's command/script safety policy, shared by the
// shell tool, the script runner and file writes. It has two parts:
//
//   - a hard DENYLIST (ScanDangerous): catastrophic patterns that are refused
//     outright. No approval prompt can authorize them.
//   - a read-only ALLOWLIST (IsReadOnlyCommand): inspection commands that are
//     safe to run without an approval prompt, reducing approval fatigue.
//
// These checks are heuristic defense-in-depth, not a true sandbox. Real
// isolation comes from running the agent in a locked-down container.

// blockedRule is a dangerous pattern that is refused outright.
type blockedRule struct {
	name string
	re   *regexp.Regexp
}

var blockedRules = []blockedRule{
	{"fork bomb", regexp.MustCompile(`:\s*\(\s*\)\s*\{[^}]*\|[^}]*&[^}]*\}\s*;\s*:`)},
	{
		// rm with a recursive flag targeting root, home or a system directory.
		"recursive delete of a system, home or absolute path",
		regexp.MustCompile(`(?i)\brm\s+-[^\s]*[rR][^\s]*\s+([^\s]+\s+)*(/|~|\$HOME|--no-preserve-root)`),
	},
	{"writing to a block device", regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|disk|hd|mmcblk)`)},
	{"dd to a device", regexp.MustCompile(`(?i)\bdd\b[^|;&]*\bof=\s*/dev/`)},
	{"filesystem format (mkfs)", regexp.MustCompile(`(?i)\bmkfs(\.[a-z0-9]+)?\b`)},
	{"system shutdown/reboot", regexp.MustCompile(`(?i)\b(shutdown|reboot|halt|poweroff)\b|\binit\s+0\b`)},
	{
		// Downloading and piping straight into an interpreter.
		"piping a network download into a shell",
		regexp.MustCompile(`(?i)\b(curl|wget|fetch)\b[^|]*\|\s*(sudo\s+)?(sh|bash|zsh|python3?|perl|ruby|node)\b`),
	},
	{
		"recursive chmod/chown of a system, home or absolute path",
		regexp.MustCompile(`(?i)\b(chmod|chown)\s+-[^\s]*[rR][^\s]*\s+([^\s]+\s+)*(/|~|\$HOME)`),
	},
	{
		"overwriting credential or system files",
		regexp.MustCompile(`(?i)>\s*(/etc/(passwd|shadow|sudoers)\b|~?/\.ssh/|~?/\.aws/)`),
	},
}

// ScanDangerous reports an error when text contains a denylisted pattern. It is
// applied to shell command lines and to script file contents alike.
func ScanDangerous(text string) error {
	for _, r := range blockedRules {
		if r.re.MatchString(text) {
			return fmt.Errorf("refused by safety policy: %s", r.name)
		}
	}
	return nil
}

// readOnlyBins are commands that cannot modify state regardless of their flags
// (no metacharacters are allowed alongside them; see IsReadOnlyCommand).
var readOnlyBins = map[string]bool{
	"ls": true, "pwd": true, "echo": true, "cat": true, "head": true,
	"tail": true, "wc": true, "grep": true, "rg": true, "which": true,
	"whoami": true, "date": true, "stat": true, "file": true, "tree": true,
	"env": true, "printenv": true, "uname": true, "df": true, "du": true,
	"ps": true, "id": true, "hostname": true, "sort": true, "uniq": true,
	"cut": true, "basename": true, "dirname": true, "realpath": true,
}

// readOnlySub maps a binary to subcommands that are read-only for ANY arguments.
// (Subcommands like "git branch" or "go env" are excluded because flags such as
// "-D" or "-w" can mutate state.)
var readOnlySub = map[string]map[string]bool{
	"git": {"status": true, "diff": true, "log": true, "show": true},
	"go":  {"version": true, "list": true, "doc": true},
}

// IsReadOnlyCommand reports whether command is a single, simple invocation that
// only inspects state, so it can run without an approval prompt. It is
// deliberately conservative: any shell metacharacter that could chain, redirect
// or expand into a mutating action disqualifies the command.
func IsReadOnlyCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return false
	}
	if strings.ContainsAny(cmd, "|&;<>`$(){}\\\n") {
		return false
	}
	fields := strings.Fields(cmd)
	bin := fields[0]
	if subs, ok := readOnlySub[bin]; ok {
		return len(fields) >= 2 && subs[fields[1]]
	}
	return readOnlyBins[bin]
}

// neverAutoApprove lists tools whose individual invocations must always be
// evaluated for approval, even when a user adds them to auto_approve. The shared
// trait is that the tool is a generic launcher whose real danger depends on the
// specific call, not the tool name:
//   - run_shell can run any command, so a blanket bypass is never permitted.
//   - run_skill only launches the sandbox; the danger lives in the particular
//     skill's script and the write/network access it declares. Auto-approving
//     the launcher would silently bypass that per-skill gate, so each call is
//     still evaluated (a remembered "always" grant is per skill + capability).
//
// toolNameRunSkill is duplicated here because the run_skill tool lives in the
// agent package; keeping the string in one place keeps this list authoritative.
const toolNameRunSkill = "run_skill"

var neverAutoApprove = map[string]bool{
	toolNameRunShell: true,
	toolNameRunSkill: true,
}

// NeverAutoApprove reports whether a tool is barred from the auto_approve list.
func NeverAutoApprove(toolName string) bool { return neverAutoApprove[toolName] }

// scriptExtensions are file types treated as executable scripts; writes to these
// are scanned with ScanDangerous so the skill writer cannot create a bad script.
var scriptExtensions = map[string]bool{
	".sh": true, ".bash": true, ".zsh": true, ".py": true,
	".pl": true, ".rb": true, ".js": true,
}

// isScriptPath reports whether path has a script file extension.
func isScriptPath(path string) bool {
	dot := strings.LastIndex(path, ".")
	if dot < 0 {
		return false
	}
	return scriptExtensions[strings.ToLower(path[dot:])]
}
