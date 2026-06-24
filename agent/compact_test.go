package agent

import (
	"context"
	"strings"
	"testing"
	"zzy/copilot"
)

func userMsg(s string) copilot.Message { return copilot.Message{Role: roleUser, Content: s} }
func asstMsg(s string) copilot.Message { return copilot.Message{Role: roleAssistant, Content: s} }

func TestSplitForCompactionUserBoundary(t *testing.T) {
	msgs := []copilot.Message{
		userMsg("u1"),
		asstMsg("a1"),
		userMsg("u2"),
		{Role: roleAssistant, ToolCalls: []copilot.ToolCall{{ID: "1", Function: copilot.ToolCallFunction{Name: "x"}}}},
		{Role: roleTool, ToolCallID: "1", Content: "result"},
		asstMsg("a2"),
	}
	// Keep 3: raw boundary is index 3 (assistant tool_calls); it must move back
	// to the user message at index 2 so the tool pair stays intact.
	older, recent, ok := splitForCompaction(msgs, 3)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(older) != 2 {
		t.Fatalf("older = %d, want 2", len(older))
	}
	if recent[0].Role != roleUser || recent[0].Content != "u2" {
		t.Fatalf("recent must start at user u2, got %+v", recent[0])
	}
}

func TestSplitForCompactionNothingToDo(t *testing.T) {
	msgs := []copilot.Message{userMsg("u1"), asstMsg("a1")}
	if _, _, ok := splitForCompaction(msgs, 3); ok {
		t.Fatal("expected ok=false when history fits in keep window")
	}
}

func TestCompactReplacesOlderWithSummary(t *testing.T) {
	e := &Engine{compactKeep: 2}
	e.summarize = func(_ context.Context, older []copilot.Message) (string, error) {
		return "SUMMARY", nil
	}
	msgs := []copilot.Message{
		userMsg("u1"), asstMsg("a1"),
		userMsg("u2"), asstMsg("a2"),
		userMsg("u3"), asstMsg("a3"),
	}
	out, ok := e.compact(context.Background(), msgs)
	if !ok {
		t.Fatal("expected compaction to happen")
	}
	if !isSummaryMessage(out[0]) {
		t.Fatalf("first message should be a summary, got %+v", out[0])
	}
	if !strings.Contains(out[0].Content, "SUMMARY") {
		t.Fatalf("summary content missing: %q", out[0].Content)
	}
	// keepRecent=2, boundary lands on user u3.
	if out[1].Content != "u3" || out[len(out)-1].Content != "a3" {
		t.Fatalf("recent window wrong: %+v", out[1:])
	}
}

func TestCompactFoldsPriorSummary(t *testing.T) {
	e := &Engine{compactKeep: 2}
	var got []copilot.Message
	e.summarize = func(_ context.Context, older []copilot.Message) (string, error) {
		got = older
		return "NEW", nil
	}
	msgs := []copilot.Message{
		{Role: roleSystem, Content: conversationSummaryPrefix + "OLD"},
		userMsg("u1"), asstMsg("a1"),
		userMsg("u2"), asstMsg("a2"),
	}
	out, ok := e.compact(context.Background(), msgs)
	if !ok {
		t.Fatal("expected compaction")
	}
	// The prior summary must be part of what gets re-summarized.
	if len(got) == 0 || !isSummaryMessage(got[0]) {
		t.Fatalf("prior summary not folded into summarize input: %+v", got)
	}
	if !strings.Contains(out[0].Content, "NEW") {
		t.Fatalf("expected new summary, got %q", out[0].Content)
	}
}

func TestCompactFailureLeavesHistoryUnchanged(t *testing.T) {
	e := &Engine{compactKeep: 2}
	e.summarize = func(_ context.Context, _ []copilot.Message) (string, error) {
		return "", context.Canceled
	}
	msgs := []copilot.Message{userMsg("u1"), asstMsg("a1"), userMsg("u2"), asstMsg("a2")}
	out, ok := e.compact(context.Background(), msgs)
	if ok {
		t.Fatal("expected ok=false on summarizer error")
	}
	if len(out) != len(msgs) {
		t.Fatalf("history changed on failure: %d != %d", len(out), len(msgs))
	}
}

func TestTrimHistoryPreservesSummary(t *testing.T) {
	msgs := []copilot.Message{
		{Role: roleSystem, Content: conversationSummaryPrefix + "S"},
		userMsg("u1"), asstMsg("a1"),
		userMsg("u2"), asstMsg("a2"),
		userMsg("u3"), asstMsg("a3"),
	}
	out := trimHistory(msgs, 3)
	if !isSummaryMessage(out[0]) {
		t.Fatalf("summary dropped by trim: %+v", out)
	}
	if out[1].Role != roleUser {
		t.Fatalf("message after summary must be a user message, got %+v", out[1])
	}
}

func TestRenderTranscriptIncludesToolActivity(t *testing.T) {
	msgs := []copilot.Message{
		userMsg("hello"),
		{Role: roleAssistant, ToolCalls: []copilot.ToolCall{{ID: "1", Function: copilot.ToolCallFunction{Name: "search", Arguments: `{"q":"x"}`}}}},
		{Role: roleTool, ToolCallID: "1", Content: "found it"},
	}
	got := renderTranscript(msgs)
	for _, want := range []string{"User: hello", "tool search", "found it"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q in:\n%s", want, got)
		}
	}
}
