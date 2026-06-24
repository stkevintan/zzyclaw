package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSearchFiles(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep (rg) not installed; skipping search_files test")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\nfoo bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, _ := NewSandbox(dir)
	tool := NewSearchFiles(sb, 10*time.Second)
	ctx := context.Background()

	out, err := tool.Execute(ctx, json.RawMessage(`{"pattern":"foo","path":"."}`))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if want := "a.txt:2:foo bar"; out == "" || out != want {
		t.Fatalf("got %q, want %q", out, want)
	}

	none, err := tool.Execute(ctx, json.RawMessage(`{"pattern":"nonexistentzzz"}`))
	if err != nil {
		t.Fatalf("search no-match: %v", err)
	}
	if none != "No matches found." {
		t.Fatalf("got %q, want no-match message", none)
	}
}

func TestSearchFilesRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	sb, _ := NewSandbox(dir)
	tool := NewSearchFiles(sb, 10*time.Second)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"x","path":"../"}`))
	if err == nil {
		t.Fatal("expected error searching outside sandbox")
	}
}
