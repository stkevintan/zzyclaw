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
		NewEditFile(sb):   true,
		NewDeletePath(sb): true,
	}
	for tool, want := range cases {
		if got := tool.Dangerous(nil); got != want {
			t.Errorf("%s.Dangerous()=%v, want %v", tool.Name(), got, want)
		}
	}
}

func TestReadFileLineRange(t *testing.T) {
	dir := t.TempDir()
	sb, _ := NewSandbox(dir)
	w := NewWriteFile(sb)
	r := NewReadFile(sb)
	ctx := context.Background()

	if _, err := w.Execute(ctx, json.RawMessage(`{"path":"a.txt","content":"l1\nl2\nl3\nl4\nl5"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := r.Execute(ctx, json.RawMessage(`{"path":"a.txt","start_line":2,"end_line":4}`))
	if err != nil {
		t.Fatalf("read range: %v", err)
	}
	if out != "l2\nl3\nl4" {
		t.Fatalf("range got %q, want %q", out, "l2\nl3\nl4")
	}

	// Only start_line → read to end.
	out, err = r.Execute(ctx, json.RawMessage(`{"path":"a.txt","start_line":4}`))
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if out != "l4\nl5" {
		t.Fatalf("tail got %q, want %q", out, "l4\nl5")
	}

	// No range → full file.
	out, _ = r.Execute(ctx, json.RawMessage(`{"path":"a.txt"}`))
	if out != "l1\nl2\nl3\nl4\nl5" {
		t.Fatalf("full got %q", out)
	}

	if _, err := r.Execute(ctx, json.RawMessage(`{"path":"a.txt","start_line":99}`)); err == nil {
		t.Fatal("expected error for start_line past EOF")
	}
}

func TestWriteFileAppend(t *testing.T) {
	dir := t.TempDir()
	sb, _ := NewSandbox(dir)
	w := NewWriteFile(sb)
	r := NewReadFile(sb)
	ctx := context.Background()

	if _, err := w.Execute(ctx, json.RawMessage(`{"path":"log.txt","content":"a"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := w.Execute(ctx, json.RawMessage(`{"path":"log.txt","content":"b","append":true}`)); err != nil {
		t.Fatalf("append: %v", err)
	}
	out, _ := r.Execute(ctx, json.RawMessage(`{"path":"log.txt"}`))
	if out != "ab" {
		t.Fatalf("append got %q, want %q", out, "ab")
	}
}

func TestEditFileReplace(t *testing.T) {
	dir := t.TempDir()
	sb, _ := NewSandbox(dir)
	w := NewWriteFile(sb)
	e := NewEditFile(sb)
	r := NewReadFile(sb)
	ctx := context.Background()

	if _, err := w.Execute(ctx, json.RawMessage(`{"path":"f.txt","content":"foo bar foo"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Ambiguous single replace is rejected.
	if _, err := e.Execute(ctx, json.RawMessage(`{"path":"f.txt","old_string":"foo","new_string":"baz"}`)); err == nil {
		t.Fatal("expected ambiguous-match error")
	}

	// Unique replace works.
	if _, err := e.Execute(ctx, json.RawMessage(`{"path":"f.txt","old_string":"foo bar","new_string":"X"}`)); err != nil {
		t.Fatalf("edit: %v", err)
	}
	out, _ := r.Execute(ctx, json.RawMessage(`{"path":"f.txt"}`))
	if out != "X foo" {
		t.Fatalf("edit got %q, want %q", out, "X foo")
	}

	// replace_all replaces every occurrence.
	if _, err := w.Execute(ctx, json.RawMessage(`{"path":"g.txt","content":"a a a"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := e.Execute(ctx, json.RawMessage(`{"path":"g.txt","old_string":"a","new_string":"b","replace_all":true}`)); err != nil {
		t.Fatalf("replace_all: %v", err)
	}
	out, _ = r.Execute(ctx, json.RawMessage(`{"path":"g.txt"}`))
	if out != "b b b" {
		t.Fatalf("replace_all got %q, want %q", out, "b b b")
	}

	// Missing old_string errors.
	if _, err := e.Execute(ctx, json.RawMessage(`{"path":"g.txt","old_string":"zzz","new_string":"y"}`)); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestEditFileBlocksDangerousScript(t *testing.T) {
	dir := t.TempDir()
	sb, _ := NewSandbox(dir)
	w := NewWriteFile(sb)
	e := NewEditFile(sb)
	ctx := context.Background()

	if _, err := w.Execute(ctx, json.RawMessage(`{"path":"run.sh","content":"#!/bin/sh\necho hi\n"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Editing in a denylisted command must be refused.
	_, err := e.Execute(ctx, json.RawMessage(`{"path":"run.sh","old_string":"echo hi","new_string":"rm -rf /"}`))
	if err == nil {
		t.Fatal("expected edit to be refused by safety policy")
	}
}
