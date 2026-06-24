package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"zzy/agent/skill"
	"zzy/agent/tools"
	"zzy/copilot"
)

const (
	roleSystem    = "system"
	roleUser      = "user"
	roleAssistant = "assistant"
	roleTool      = "tool"
)

// EngineConfig configures the ReAct engine.
type EngineConfig struct {
	MaxIterations int
	MaxHistory    int
	// CompactThreshold is the stored-history length (in messages) past which a
	// completed turn triggers compaction: older messages are summarized into a
	// single note kept at the head of history. 0 disables auto-compaction.
	CompactThreshold int
	// CompactKeep is the number of most-recent messages preserved verbatim when
	// compacting; everything before them is folded into the summary.
	CompactKeep int
	AutoApprove []string
	// Owners are user IDs allowed to run dangerous (approval-gated) tools. When
	// empty, owner gating is disabled and any user may approve dangerous actions.
	Owners  []string
	Persona string
	// Grants persists "always approve" decisions so repeated equivalent dangerous
	// actions don't re-prompt. When nil, approvals are never remembered.
	Grants GrantStore
}

// Decision is a user's response to an approval prompt for a dangerous action.
type Decision int

const (
	// DecisionDeny rejects the pending action.
	DecisionDeny Decision = iota
	// DecisionApprove runs the pending action once without remembering it.
	DecisionApprove
	// DecisionAlways runs the pending action and remembers its grant scope so
	// future equivalent calls skip the prompt.
	DecisionAlways
)

// Engine runs the Reasoning-and-Acting loop: it repeatedly asks the model what
// to do, executes any requested tool calls, and feeds the results back until the
// model produces a final answer (or a dangerous action requires approval).
type Engine struct {
	client *copilot.Client
	tools  *tools.Registry
	skills *skill.Manager
	store  Store

	maxIter     int
	maxHistory  int
	compactAt   int
	compactKeep int
	autoApprove map[string]struct{}
	owners      map[string]struct{}
	persona     string
	grants      GrantStore

	// summarize condenses an older slice of messages into a compact note. It is
	// a field so tests can inject a deterministic summarizer; by default it calls
	// the model via summarizeWithModel.
	summarize func(ctx context.Context, older []copilot.Message) (string, error)
}

// NewEngine constructs an engine from its dependencies and config.
func NewEngine(client *copilot.Client, toolReg *tools.Registry, skillReg *skill.Manager, store Store, cfg EngineConfig) *Engine {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 12
	}
	if cfg.MaxHistory <= 0 {
		cfg.MaxHistory = 40
	}
	if cfg.CompactKeep <= 0 {
		cfg.CompactKeep = 12
	}
	// Auto-compaction can only shrink history when the trigger sits above the
	// verbatim window; otherwise splitForCompaction never finds anything to fold.
	if cfg.CompactThreshold > 0 && cfg.CompactThreshold <= cfg.CompactKeep {
		cfg.CompactThreshold = cfg.CompactKeep + 1
	}
	auto := make(map[string]struct{}, len(cfg.AutoApprove))
	for _, name := range cfg.AutoApprove {
		// Some tools (e.g. run_shell) are too powerful to auto-approve wholesale;
		// their individual calls are still evaluated per-command.
		if tools.NeverAutoApprove(name) {
			continue
		}
		auto[name] = struct{}{}
	}
	persona := cfg.Persona
	if persona == "" {
		persona = defaultPersona
	}
	owners := make(map[string]struct{}, len(cfg.Owners))
	for _, id := range cfg.Owners {
		if id != "" {
			owners[id] = struct{}{}
		}
	}
	grants := cfg.Grants
	if grants == nil {
		grants = NewNoopGrantStore()
	}
	e := &Engine{
		client:      client,
		tools:       toolReg,
		skills:      skillReg,
		store:       store,
		maxIter:     cfg.MaxIterations,
		maxHistory:  cfg.MaxHistory,
		compactAt:   cfg.CompactThreshold,
		compactKeep: cfg.CompactKeep,
		autoApprove: auto,
		owners:      owners,
		persona:     persona,
		grants:      grants,
	}
	e.summarize = e.summarizeWithModel
	return e
}

const defaultPersona = `You are a helpful, capable general assistant operating over a chat interface.
Reason step by step and use the available tools to accomplish the user's request.
When a tool can answer a question or perform an action, call it rather than guessing.
Prefer the structured workspace tools for file work: use read_file (with start_line/end_line
for large files), list_dir and search_files to inspect, and write_file or edit_file to change
files. Do NOT use the shell for plain file operations like cat, ls, sed or awk. Reserve run_shell
for things those tools cannot do: building, running, linting and testing code, and multi-step
commands. After changing code, run it or its tests with run_shell (e.g. "go test ./...",
"python main.py") and fix any failures before reporting success.
You can extend your own capabilities by creating skills (see the list_skills/load_skill tools).
Keep replies concise and written in the user's language.`

// Outcome is the result of a turn.
type Outcome struct {
	// Reply is the text to send to the user.
	Reply string
	// Suspended is true when the turn paused awaiting approval of a dangerous
	// action; the session's Pending field holds the resumable state.
	Suspended bool
}

// Run starts a new turn for the given user input.
func (e *Engine) Run(ctx context.Context, sess *Session, userText string) (Outcome, error) {
	messages := append([]copilot.Message(nil), sess.History...)
	messages = append(messages, copilot.Message{Role: roleUser, Content: userText})
	return e.loop(ctx, sess, messages)
}

// Resume continues a turn that was paused for approval, applying the user's
// decision to the pending tool call.
func (e *Engine) Resume(ctx context.Context, sess *Session, decision Decision) (Outcome, error) {
	p := sess.Pending
	sess.Pending = nil
	if p == nil {
		return Outcome{Reply: "There is nothing awaiting approval."}, nil
	}

	messages := p.Messages
	if decision == DecisionDeny {
		messages = append(messages, toolResult(p.Call.ID, "The user denied this action. Do not retry it; continue without it or ask how to proceed."))
	} else {
		// Remember the scope first so that, even if the tool itself fails, the
		// user's "always" choice persists for subsequent calls.
		if decision == DecisionAlways && p.GrantKey != "" {
			if err := e.grants.Grant(ctx, sess.UserID, p.GrantKey); err != nil {
				slog.Warn("failed to persist approval grant", "key", p.GrantKey, "error", err)
			}
		}
		if tool, ok := e.tools.Get(p.Call.Function.Name); ok {
			result := e.exec(ctx, sess, tool, p.Call)
			messages = append(messages, toolResult(p.Call.ID, result))
		} else {
			messages = append(messages, toolResult(p.Call.ID, "error: unknown tool"))
		}
	}

	// Process any remaining tool calls from the same assistant message.
	suspended, prompt, out := e.processBatch(ctx, sess, messages, p.Queue, 0)
	if suspended {
		return Outcome{Reply: prompt, Suspended: true}, nil
	}
	return e.loop(ctx, sess, out)
}

// loop drives the model/tool cycle until a final answer or a suspension.
func (e *Engine) loop(ctx context.Context, sess *Session, messages []copilot.Message) (Outcome, error) {
	specs := e.specs()
	for iter := 0; iter < e.maxIter; iter++ {
		res, err := e.client.ChatWithTools(ctx, e.fullMessages(sess, messages), specs)
		if err != nil {
			return Outcome{}, err
		}

		// When log.level=debug, dump the model's completion (final text and/or the
		// tool calls it wants to make) so a turn can be traced step by step.
		slog.Debug("agent completion",
			"iter", iter,
			"content", res.Content,
			"tool_calls", toolCallsLog(res.ToolCalls),
		)

		if len(res.ToolCalls) == 0 {
			messages = append(messages, copilot.Message{Role: roleAssistant, Content: res.Content})
			e.persist(ctx, sess, messages)
			return Outcome{Reply: res.Content}, nil
		}

		messages = append(messages, copilot.Message{
			Role:      roleAssistant,
			Content:   res.Content,
			ToolCalls: res.ToolCalls,
		})

		suspended, prompt, out := e.processBatch(ctx, sess, messages, res.ToolCalls, 0)
		messages = out
		if suspended {
			return Outcome{Reply: prompt, Suspended: true}, nil
		}
	}
	return Outcome{Reply: "I wasn't able to finish within the allowed number of steps. Please refine your request."}, nil
}

// processBatch executes tool calls in order starting at startIdx. If it hits a
// dangerous call that is not auto-approved, it records the pending state on the
// session and returns suspended=true with an approval prompt.
func (e *Engine) processBatch(ctx context.Context, sess *Session, messages []copilot.Message, calls []copilot.ToolCall, startIdx int) (bool, string, []copilot.Message) {
	for idx := startIdx; idx < len(calls); idx++ {
		call := calls[idx]
		tool, ok := e.tools.Get(call.Function.Name)
		if !ok {
			messages = append(messages, toolResult(call.ID, fmt.Sprintf("error: unknown tool %q", call.Function.Name)))
			continue
		}

		args := json.RawMessage(call.Function.Arguments)
		if tool.Dangerous(e.toolCtx(ctx, sess), args) {
			if !e.ownerAllowed(sess) {
				messages = append(messages, toolResult(call.ID,
					"refused: this action requires elevated permission and the current user is not an authorized owner"))
				continue
			}
			// A previously remembered ("always") grant for this scope skips the prompt.
			grantKey, grantLabel := grantScope(e.toolCtx(ctx, sess), tool, args)
			if grantKey != "" && e.grants.Allowed(ctx, sess.UserID, grantKey) {
				result := e.exec(ctx, sess, tool, call)
				messages = append(messages, toolResult(call.ID, result))
				continue
			}
			if _, auto := e.autoApprove[call.Function.Name]; !auto {
				desc := describeCall(call)
				sess.Pending = &PendingApproval{
					Messages:    messages,
					Call:        call,
					Queue:       append([]copilot.ToolCall(nil), calls[idx+1:]...),
					Description: desc,
					GrantKey:    grantKey,
					GrantLabel:  grantLabel,
				}
				return true, approvalPrompt(desc, grantLabel), messages
			}
		}

		result := e.exec(ctx, sess, tool, call)
		messages = append(messages, toolResult(call.ID, result))
	}
	return false, "", messages
}

// grantScope returns the persistable approval scope for a tool call, or empty
// strings when the tool doesn't support remembered approvals.
func grantScope(ctx context.Context, tool tools.Tool, args json.RawMessage) (key, label string) {
	g, ok := tool.(tools.Grantable)
	if !ok {
		return "", ""
	}
	k, l, ok := g.GrantScope(ctx, args)
	if !ok {
		return "", ""
	}
	return k, l
}

// approvalPrompt builds the approval message. When the action can be remembered
// (grantLabel is set), it offers an "always" option scoped to that resource.
func approvalPrompt(desc, grantLabel string) string {
	if grantLabel != "" {
		return fmt.Sprintf("⚠️ I need your approval to run %s\n\nReply \"yes\" to allow once, \"always\" to allow and remember %s, or \"no\" to cancel.", desc, grantLabel)
	}
	return fmt.Sprintf("⚠️ I need your approval to run %s\n\nReply \"yes\" to approve or \"no\" to cancel.", desc)
}

// ownerAllowed reports whether the session's user may run dangerous tools. When
// no owners are configured the gate is disabled and everyone is allowed.
func (e *Engine) ownerAllowed(sess *Session) bool {
	if len(e.owners) == 0 {
		return true
	}
	if sess == nil {
		return false
	}
	_, ok := e.owners[sess.UserID]
	return ok
}

// toolCtx returns a context carrying the active session and user so tools can
// resolve per-user resources (e.g. skills, workspace) during both their
// Dangerous check and execution.
func (e *Engine) toolCtx(ctx context.Context, sess *Session) context.Context {
	return tools.WithUser(withSession(ctx, sess), sess.UserID)
}

// exec runs a tool with the session injected into the context.
func (e *Engine) exec(ctx context.Context, sess *Session, tool tools.Tool, call copilot.ToolCall) string {
	slog.Debug("tool call", "tool", call.Function.Name, "args", call.Function.Arguments)
	tctx := e.toolCtx(ctx, sess)
	out, err := tool.Execute(tctx, json.RawMessage(call.Function.Arguments))
	if err != nil {
		slog.Debug("tool result", "tool", call.Function.Name, "error", err.Error())
		return "Error: " + err.Error()
	}
	slog.Debug("tool result", "tool", call.Function.Name, "output", out)
	if strings.TrimSpace(out) == "" {
		return "(no output)"
	}
	return out
}

// toolCallsLog renders tool calls as compact JSON for debug logging. It
// implements slog.LogValuer so the marshaling is deferred until slog actually
// emits the record (i.e. only when debug logging is enabled).
type toolCallsLog []copilot.ToolCall

func (c toolCallsLog) LogValue() slog.Value {
	if len(c) == 0 {
		return slog.StringValue("")
	}
	b, err := json.Marshal([]copilot.ToolCall(c))
	if err != nil {
		return slog.StringValue(fmt.Sprintf("%v", []copilot.ToolCall(c)))
	}
	return slog.StringValue(string(b))
}

// fullMessages prepends the dynamic system prompt to the conversation.
func (e *Engine) fullMessages(sess *Session, messages []copilot.Message) []copilot.Message {
	out := make([]copilot.Message, 0, len(messages)+1)
	out = append(out, copilot.Message{Role: roleSystem, Content: e.systemPrompt(sess)})
	out = append(out, messages...)
	return out
}

// systemPrompt assembles the persona, the catalog of available skills, and the
// instructions of any skills currently loaded in the session.
func (e *Engine) systemPrompt(sess *Session) string {
	var b strings.Builder
	b.WriteString(e.persona)

	userID := ""
	if sess != nil {
		userID = sess.UserID
	}
	if skills := e.skills.List(userID); len(skills) > 0 {
		b.WriteString("\n\n# Available skills\nLoad a skill with load_skill when its description matches the task:\n")
		for _, s := range skills {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
	}

	if loaded := loadedSkillInstructions(e.skills, sess); len(loaded) > 0 {
		b.WriteString("\n# Loaded skill instructions\n")
		b.WriteString(strings.Join(loaded, "\n\n"))
	}
	return b.String()
}

// specs converts the registered tools into Copilot tool specifications.
func (e *Engine) specs() []copilot.Tool {
	all := e.tools.All()
	specs := make([]copilot.Tool, 0, len(all))
	for _, t := range all {
		specs = append(specs, copilot.Tool{
			Type: "function",
			Function: copilot.ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema(),
			},
		})
	}
	return specs
}

// persist compacts (when configured) and trims history, then saves it.
func (e *Engine) persist(ctx context.Context, sess *Session, messages []copilot.Message) {
	if e.compactAt > 0 && len(messages) > e.compactAt {
		if compacted, ok := e.compact(ctx, messages); ok {
			messages = compacted
		}
	}
	trimmed := trimHistory(messages, e.maxHistory)
	sess.History = trimmed
	if err := e.store.Save(ctx, sess.Key, trimmed); err != nil {
		// Persistence is best-effort; the in-memory session still holds history.
		_ = err
	}
}

// toolResult builds a tool-role message carrying a tool call's output.
func toolResult(callID, content string) copilot.Message {
	return copilot.Message{Role: roleTool, ToolCallID: callID, Content: content}
}

// describeCall renders a tool call as a short human-readable string for approval.
func describeCall(call copilot.ToolCall) string {
	args := strings.TrimSpace(call.Function.Arguments)
	if len(args) > 400 {
		args = args[:400] + "…"
	}
	if args == "" || args == "{}" {
		return fmt.Sprintf("`%s`", call.Function.Name)
	}
	return fmt.Sprintf("`%s` with arguments: %s", call.Function.Name, args)
}

// trimHistory caps the history to the most recent maxHistory messages and drops
// any leading orphan messages so the slice begins on a user message (a tool
// message must always follow its assistant tool_calls message). A leading
// compaction summary is always preserved and never counts as an orphan.
func trimHistory(messages []copilot.Message, maxHistory int) []copilot.Message {
	// Detach a leading summary so trimming never discards the compacted context.
	var summary []copilot.Message
	if len(messages) > 0 && isSummaryMessage(messages[0]) {
		summary = messages[:1]
		messages = messages[1:]
	}
	budget := maxHistory - len(summary)
	if budget < 1 {
		budget = 1
	}
	if len(messages) > budget {
		messages = messages[len(messages)-budget:]
	}
	start := 0
	for start < len(messages) {
		if messages[start].Role == roleUser {
			break
		}
		start++
	}
	if start > 0 {
		messages = messages[start:]
	}
	out := make([]copilot.Message, 0, len(summary)+len(messages))
	out = append(out, summary...)
	out = append(out, messages...)
	return out
}
