package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSandboxRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	sb, err := NewSandbox(dir)
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	w := NewWriteFile(sb)
	_, err = w.Execute(context.Background(), json.RawMessage(`{"path":"../escape.txt","content":"x"}`))
	if err == nil {
		t.Fatal("expected error writing outside sandbox, got nil")
	}
}

func TestSandboxRejectsAbsoluteOutside(t *testing.T) {
	dir := t.TempDir()
	sb, _ := NewSandbox(dir)
	r := NewReadFile(sb)
	_, err := r.Execute(context.Background(), json.RawMessage(`{"path":"/etc/passwd"}`))
	if err == nil {
		t.Fatal("expected error reading absolute path outside sandbox, got nil")
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sb, _ := NewSandbox(dir)
	w := NewWriteFile(sb)
	r := NewReadFile(sb)
	ctx := context.Background()

	if _, err := w.Execute(ctx, json.RawMessage(`{"path":"sub/a.txt","content":"hello"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := r.Execute(ctx, json.RawMessage(`{"path":"sub/a.txt"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "hello" {
		t.Fatalf("got %q, want %q", out, "hello")
	}
}

func TestDangerFlags(t *testing.T) {
	dir := t.TempDir()
	sb, _ := NewSandbox(dir)
	cases := map[Tool]bool{
		NewReadFile(sb):   false,
		NewListDir(sb):    false,
		NewWriteFile(sb):  true,
		NewDeletePath(sb): true,
	}
	for tool, want := range cases {
		if got := tool.Dangerous(nil); got != want {
			t.Errorf("%s.Dangerous()=%v, want %v", tool.Name(), got, want)
		}
	}
}
