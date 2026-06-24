package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDenoArgsDefaultDenyByDefault(t *testing.T) {
	argv := denoArgs("/skills/x/skill.js", nil, DenoPermissions{})
	joined := strings.Join(argv, " ")
	for _, want := range []string{"run", "--no-prompt", "--no-remote", "/skills/x/skill.js"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v missing %q", argv, want)
		}
	}
	// No permission flags when nothing is granted.
	for _, bad := range []string{"--allow-read", "--allow-write", "--allow-net"} {
		if strings.Contains(joined, bad) {
			t.Errorf("argv %v unexpectedly granted %q", argv, bad)
		}
	}
}

func TestDenoArgsGrantsScopedPermissions(t *testing.T) {
	perms := DenoPermissions{
		Read:  []string{"/skills/x", "/work"},
		Write: []string{"/work"},
		Net:   []string{"example.com", "api.example.org"},
	}
	argv := denoArgs("/skills/x/skill.ts", []string{"a", "b"}, perms)
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
	// Script args come after the entry file.
	if argv[len(argv)-2] != "a" || argv[len(argv)-1] != "b" {
		t.Errorf("script args not appended last: %v", argv)
	}
}

func TestDenoRunnerNotInstalled(t *testing.T) {
	r := NewDenoRunner("no-such-deno-binary-xyz", t.TempDir(), time.Second)
	if r.Installed() {
		t.Skip("unexpected deno binary on PATH named no-such-deno-binary-xyz")
	}
	if _, err := r.Run(context.Background(), "/tmp/skill.js", nil, DenoPermissions{}); err == nil {
		t.Fatal("expected error when deno is not installed")
	}
}
