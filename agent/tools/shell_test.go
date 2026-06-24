package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestShellRunsCommand(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	sh := NewShell(sb, 10*time.Second)
	out, err := sh.Execute(context.Background(), json.RawMessage(`{"command":"echo hello"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected output to contain hello, got %q", out)
	}
}

func TestShellSurfacesNonZeroExit(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	sh := NewShell(sb, 10*time.Second)
	out, err := sh.Execute(context.Background(), json.RawMessage(`{"command":"echo boom >&2; exit 3"}`))
	if err != nil {
		t.Fatalf("non-zero exit should not be a Go error, got %v", err)
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "exit code 3") {
		t.Fatalf("expected output and exit code, got %q", out)
	}
}

func TestShellRejectsCwdOutsideSandbox(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	sh := NewShell(sb, 10*time.Second)
	_, err := sh.Execute(context.Background(), json.RawMessage(`{"command":"pwd","cwd":"../"}`))
	if err == nil {
		t.Fatal("expected error for cwd outside sandbox, got nil")
	}
}

func TestShellTimeout(t *testing.T) {
	sb, _ := NewSandbox(t.TempDir())
	sh := NewShell(sb, 100*time.Millisecond)
	_, err := sh.Execute(context.Background(), json.RawMessage(`{"command":"sleep 5"}`))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}
