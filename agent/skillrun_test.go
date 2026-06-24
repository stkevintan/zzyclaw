package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zzy/agent/skill"
	"zzy/agent/tools"
)

func TestRunSkillGating(t *testing.T) {
	dir := t.TempDir()

	// A read-only, no-network deno skill with an entry file.
	wdir := filepath.Join(dir, "greet")
	mustMkdir(t, wdir)
	mustWrite(t, filepath.Join(wdir, "SKILL.md"), "---\nname: greet\ndescription: x\nruntime: deno\nentry: skill.js\n---\n# Greet\n")
	mustWrite(t, filepath.Join(wdir, "skill.js"), "console.log('hi')\n")

	// A deno skill that requests network — running it is dangerous (needs approval).
	netdir := filepath.Join(dir, "fetcher")
	mustMkdir(t, netdir)
	mustWrite(t, filepath.Join(netdir, "SKILL.md"), "---\nname: fetcher\ndescription: x\nruntime: deno\nnet: example.com\n---\n# Fetcher\n")
	mustWrite(t, filepath.Join(netdir, "skill.js"), "console.log('net')\n")

	// An instructions-only skill (no runtime) — not runnable via run_skill.
	ndir := filepath.Join(dir, "doc")
	mustMkdir(t, ndir)
	mustWrite(t, filepath.Join(ndir, "SKILL.md"), "---\nname: doc\ndescription: y\n---\n# Doc\n")

	mgr, err := skill.NewManager(dir, nil)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	if err := mgr.Reload(""); err != nil {
		t.Fatalf("reload: %v", err)
	}

	runner := tools.NewDenoRunner(filepath.Join(dir, "no-such-deno-binary"), filepath.Join(dir, "cache"), time.Second) // not installed
	tool := RunSkillTool(mgr, runner, "")

	// Read-only, no-network skill: frictionless (not dangerous).
	if tool.Dangerous(context.Background(), json.RawMessage(`{"skill":"greet"}`)) {
		t.Error("read-only no-network skill must not be dangerous")
	}
	// Network skill: requires approval.
	if !tool.Dangerous(context.Background(), json.RawMessage(`{"skill":"fetcher"}`)) {
		t.Error("network skill must be dangerous (needs approval)")
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"skill":"nope"}`)); err == nil {
		t.Fatal("expected unknown-skill error")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"skill":"doc"}`)); err == nil {
		t.Fatal("expected error: instructions-only skill is not runtime:deno")
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"skill":"greet"}`)); err == nil {
		t.Fatal("expected deno-not-installed error")
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
