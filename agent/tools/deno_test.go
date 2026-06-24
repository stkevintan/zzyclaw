package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDenoArgsDefaultDenyByDefault(t *testing.T) {
	argv := denoArgs("/skills/x/skill.js", nil, DenoPermissions{}, 0)
	joined := strings.Join(argv, " ")
	for _, want := range []string{"run", "--no-prompt", "--no-remote", "/skills/x/skill.js"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v missing %q", argv, want)
		}
	}
	// No permission flags when nothing is granted.
	for _, bad := range []string{"--allow-read", "--allow-write", "--allow-net", "--allow-env"} {
		if strings.Contains(joined, bad) {
			t.Errorf("argv %v unexpectedly granted %q", argv, bad)
		}
	}
	// No heap cap flag when maxOldSpaceMB is 0.
	if strings.Contains(joined, "--v8-flags") {
		t.Errorf("argv %v unexpectedly set a v8 flag", argv)
	}
}

func TestDenoArgsGrantsScopedPermissions(t *testing.T) {
	perms := DenoPermissions{
		Read:  []string{"/skills/x", "/work"},
		Write: []string{"/work"},
		Net:   []string{"example.com", "api.example.org"},
		Env:   []string{"API_TOKEN", "HOME"},
	}
	argv := denoArgs("/skills/x/skill.ts", []string{"a", "b"}, perms, 0)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--allow-read=/skills/x,/work") {
		t.Errorf("missing scoped read in %v", argv)
	}
	if !strings.Contains(joined, "--allow-write=/work") {
		t.Errorf("missing scoped write in %v", argv)
	}
	if !strings.Contains(joined, "--allow-net=example.com,api.example.org") {
		t.Errorf("missing scoped net in %v", argv)
	}
	if !strings.Contains(joined, "--allow-env=API_TOKEN,HOME") {
		t.Errorf("missing scoped env in %v", argv)
	}
	// Script args come after the entry file.
	if argv[len(argv)-2] != "a" || argv[len(argv)-1] != "b" {
		t.Errorf("script args not appended last: %v", argv)
	}
}

func TestDenoArgsEnvAlwaysScoped(t *testing.T) {
	// env access is always scoped to declared names; never a bare --allow-env.
	argv := denoArgs("/skills/x/skill.js", nil, DenoPermissions{Env: []string{"API_TOKEN"}}, 0)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--allow-env=API_TOKEN") {
		t.Errorf("missing scoped env in %v", argv)
	}
	for _, a := range argv {
		if a == "--allow-env" {
			t.Errorf("env must never be granted unscoped (bare --allow-env): %v", argv)
		}
	}
}

func TestDenoArgsNetWildcardGrantsAll(t *testing.T) {
	argv := denoArgs("/skills/x/skill.js", nil, DenoPermissions{Net: []string{"*"}}, 0)
	joined := strings.Join(argv, " ")
	for _, a := range argv {
		if strings.HasPrefix(a, "--allow-net=") {
			t.Errorf("net=* should grant all network via bare --allow-net, got %q", a)
		}
	}
	if !strings.Contains(joined, "--allow-net") {
		t.Errorf("missing --allow-net for net=* in %v", argv)
	}
}

func TestDenoArgsMemoryCap(t *testing.T) {
	argv := denoArgs("/skills/x/skill.js", nil, DenoPermissions{}, 256)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--v8-flags=--max-old-space-size=256") {
		t.Errorf("missing heap cap flag in %v", argv)
	}
	// The cap is a Deno runtime flag and must precede the entry file.
	entry := -1
	capIdx := -1
	for i, a := range argv {
		if a == "/skills/x/skill.js" {
			entry = i
		}
		if strings.HasPrefix(a, "--v8-flags=") {
			capIdx = i
		}
	}
	if capIdx < 0 || entry < 0 || capIdx > entry {
		t.Errorf("v8 flag must come before the entry file: %v", argv)
	}
}

func TestDenoRunnerNotInstalled(t *testing.T) {
	r := NewDenoRunner("no-such-deno-binary-xyz", t.TempDir(), time.Second, 0)
	if r.Installed() {
		t.Skip("unexpected deno binary on PATH named no-such-deno-binary-xyz")
	}
	if _, err := r.Run(context.Background(), "/tmp/skill.js", nil, DenoPermissions{}); err == nil {
		t.Fatal("expected error when deno is not installed")
	}
}
