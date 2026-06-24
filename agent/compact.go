package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"zzy/copilot"
)

// conversationSummaryPrefix marks a synthetic system message that holds the
// compacted summary of older conversation turns. The prefix lets the engine
// recognize an existing summary so it can be preserved by trimming and folded
// into the next compaction.
const conversationSummaryPrefix = "[Earlier conversation summary]\n"

// summarySystemPrompt instructs the model how to condense a conversation. The
// goal is a faithful, reusable context rather than a user-facing recap.
const summarySystemPrompt = `You compact a chat conversation to preserve context while reducing its length.
Summarize the conversation below into concise but complete notes that retain:
- the user's goals, requests, and constraints
- important facts, decisions, and outcomes (including key tool results)
- the current state and any unfinished tasks or next steps
Write neutral notes, not a dialogue. Keep specifics: names, IDs, file paths, numbers and exact values.
Do not add greetings or commentary. Respond in the language of the conversation.`

// maxRenderedField bounds how much of any single message (assistant content,
// tool arguments or tool output) is fed to the summarizer, so one huge payload
// can't dominate the summary request.
const maxRenderedField = 2000

// isSummaryMessage reports whether m is a compaction summary produced by compact.
func isSummaryMessage(m copilot.Message) bool {
	return m.Role == roleSystem && strings.HasPrefix(m.Content, conversationSummaryPrefix)
}

// compact summarizes the older portion of messages into a single summary message
// kept at the head, preserving the most recent compactKeep messages verbatim. It
// returns ok=false (and the input unchanged) when there is nothing to compact or
// the summarizer fails, so callers can fall back to plain trimming.
func (e *Engine) compact(ctx context.Context, messages []copilot.Message) ([]copilot.Message, bool) {
	older, recent, ok := splitForCompaction(messages, e.compactKeep)
	if !ok {
		return messages, false
	}
	summary, err := e.summarize(ctx, older)
	if err != nil {
		slog.Warn("conversation compaction failed", "error", err)
		return messages, false
	}
	if summary = strings.TrimSpace(summary); summary == "" {
		return messages, false
	}
	out := make([]copilot.Message, 0, len(recent)+1)
	out = append(out, copilot.Message{Role: roleSystem, Content: conversationSummaryPrefix + summary})
	out = append(out, recent...)
	// Bail out if the summary didn't actually shorten the history: compacting
	// would only spend tokens and lose precise messages for no gain.
	if len(out) >= len(messages) {
		return messages, false
	}
	slog.Debug("conversation compacted", "before", len(messages), "after", len(out))
	return out, true
}

// CompactSession compacts the session's stored history immediately and persists
// the result. It returns the message counts before and after. When there is
// nothing to compact, before == after and no write occurs.
func (e *Engine) CompactSession(ctx context.Context, sess *Session) (before, after int, err error) {
	before = len(sess.History)
	compacted, ok := e.compact(ctx, sess.History)
	if !ok {
		return before, before, nil
	}
	compacted = trimHistory(compacted, e.maxHistory)
	sess.History = compacted
	if e.store != nil {
		if err := e.store.Save(ctx, sess.Key, compacted); err != nil {
			return before, len(compacted), err
		}
	}
	return before, len(compacted), nil
}

// splitForCompaction divides messages into an older prefix to summarize and a
// recent suffix to keep verbatim. It keeps at least keepRecent messages, then
// extends the recent window backward to the nearest user-message boundary so a
// tool message is never separated from the assistant tool_calls it answers. It
// returns ok=false when there is nothing that can be compacted without orphaning.
func splitForCompaction(messages []copilot.Message, keepRecent int) (older, recent []copilot.Message, ok bool) {
	if keepRecent < 1 {
		keepRecent = 1
	}
	if len(messages) <= keepRecent {
		return nil, nil, false
	}
	split := len(messages) - keepRecent
	// Move the boundary back so the recent window begins on a user message.
	for split > 0 && messages[split].Role != roleUser {
		split--
	}
	if split <= 0 {
		return nil, nil, false
	}
	return messages[:split], messages[split:], true
}

// summarizeWithModel renders the older messages to a transcript and asks the
// model for a compact summary. It is the default Engine.summarize implementation.
func (e *Engine) summarizeWithModel(ctx context.Context, older []copilot.Message) (string, error) {
	transcript := renderTranscript(older)
	if strings.TrimSpace(transcript) == "" {
		return "", nil
	}
	return e.client.Chat(ctx, []copilot.Message{
		{Role: roleSystem, Content: summarySystemPrompt},
		{Role: roleUser, Content: "Conversation to summarize:\n\n" + transcript},
	})
}

// renderTranscript flattens messages into a plain-text transcript for the
// summarizer, including tool calls and results and any prior summary.
func renderTranscript(messages []copilot.Message) string {
	var b strings.Builder
	for _, m := range messages {
		switch m.Role {
		case roleSystem:
			if isSummaryMessage(m) {
				fmt.Fprintf(&b, "Earlier summary: %s\n\n", strings.TrimPrefix(m.Content, conversationSummaryPrefix))
			}
		case roleUser:
			fmt.Fprintf(&b, "User: %s\n\n", clip(m.Content))
		case roleAssistant:
			if strings.TrimSpace(m.Content) != "" {
				fmt.Fprintf(&b, "Assistant: %s\n\n", clip(m.Content))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "Assistant called tool %s(%s)\n\n", tc.Function.Name, clip(tc.Function.Arguments))
			}
		case roleTool:
			fmt.Fprintf(&b, "Tool result: %s\n\n", clip(m.Content))
		}
	}
	return strings.TrimSpace(b.String())
}

// clip truncates s to maxRenderedField runes, appending an ellipsis when cut.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxRenderedField {
		return string(r[:maxRenderedField]) + "…"
	}
	return s
}
