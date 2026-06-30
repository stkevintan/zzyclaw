package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"zzy/agent/skill"
	"zzy/copilot"
	"zzy/middlewares"

	wechatbot "github.com/corespeed-io/wechatbot/golang"
)

// Middleware is the chat entry point for the general agent. It owns no
// per-bot state beyond the bot handle; sessions, the engine and the skill
// registry are shared across bots.
type Middleware struct {
	middlewares.BotClient

	engine    *Engine
	sessions  *SessionManager
	skills    *skill.Manager
	reflector *Reflector
	router    *middlewares.CommandRouter
}

// NewMiddleware wires the agent middleware for a bot. reflector may be nil when
// structural memory is disabled.
func NewMiddleware(bot *wechatbot.Bot, engine *Engine, sessions *SessionManager, skills *skill.Manager, reflector *Reflector) *Middleware {
	m := &Middleware{
		BotClient: middlewares.BotClient{Bot: bot},
		engine:    engine,
		sessions:  sessions,
		skills:    skills,
		reflector: reflector,
	}
	m.router = m.buildRouter()
	return m
}

var _ middlewares.Middleware = (*Middleware)(nil)

func (m *Middleware) Name() string { return "agent" }

// Priority is low so command-style middlewares run first; the agent is the
// general fallback for free-form text.
func (m *Middleware) Priority() int { return 100 }

func (m *Middleware) HandleMessage(ctx context.Context, msg *wechatbot.IncomingMessage) bool {
	if msg.Type != wechatbot.ContentText {
		return false
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return false
	}

	if handled := m.dispatchCommand(ctx, msg, text); handled {
		return true
	}

	sess := m.sessions.Current(ctx, msg.UserID)

	sess.Mu.Lock()
	defer sess.Mu.Unlock()

	if sess.Pending != nil {
		decision, ok := parseDecision(text)
		if !ok {
			m.Reply(ctx, msg, "请回复 \"yes\" 批准本次，\"always\" 批准并记住，或 \"no\" 取消上一步操作。")
			return true
		}
		outcome, err := m.engine.Resume(ctx, sess, decision)
		m.scheduleReflection(sess, outcome, err)
		m.respond(ctx, msg, outcome, err)
		return true
	}

	firstTurn := len(sess.History) == 0
	outcome, err := m.engine.Run(ctx, sess, text)
	if err == nil && firstTurn {
		m.sessions.UpdateTitle(ctx, msg.UserID, sess.ID, text)
	}
	m.scheduleReflection(sess, outcome, err)
	m.respond(ctx, msg, outcome, err)
	return true
}

// scheduleReflection arms idle structural-memory reflection on a completed turn.
// It snapshots history under the held session lock so the background pass works
// on a fork and never blocks the live conversation.
func (m *Middleware) scheduleReflection(sess *Session, outcome Outcome, err error) {
	if m.reflector == nil || err != nil || outcome.Suspended {
		return
	}
	snapshot := append([]copilot.Message(nil), sess.History...)
	m.reflector.Schedule(sess.UserID, sess.Key, snapshot)
}

// respond sends the outcome (or an error notice) back to the user.
func (m *Middleware) respond(ctx context.Context, msg *wechatbot.IncomingMessage, outcome Outcome, err error) {
	if err != nil {
		slog.Error("agent turn failed", "user_id", msg.UserID, "error", err)
		m.Reply(ctx, msg, "处理消息失败，请稍后重试。")
		return
	}
	reply := strings.TrimSpace(outcome.Reply)
	if reply == "" {
		reply = "（无回复）"
	}
	m.ReplyChunks(ctx, msg, reply)
}

func (m *Middleware) skillList(ctx context.Context, userID string) string {
	_ = m.skills.Reload(userID)
	sess := m.sessions.Current(ctx, userID)
	sess.Mu.Lock()
	defer sess.Mu.Unlock()
	skills := m.skills.List(userID)
	if len(skills) == 0 {
		return "暂无可用技能。"
	}
	var b strings.Builder
	b.WriteString("可用技能：\n")
	for _, s := range skills {
		loaded := ""
		if _, ok := sess.ActiveSkills[s.Name]; ok {
			loaded = "（已加载）"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", s.Name, loaded, s.Description)
	}
	return strings.TrimSpace(b.String())
}

func (m *Middleware) skillLoad(ctx context.Context, msg *wechatbot.IncomingMessage, name string) {
	_ = m.skills.Reload(msg.UserID)
	if _, ok := m.skills.Get(msg.UserID, name); !ok {
		m.Reply(ctx, msg, "未找到技能："+name)
		return
	}
	sess := m.sessions.Current(ctx, msg.UserID)
	sess.Mu.Lock()
	sess.ActiveSkills[name] = struct{}{}
	sess.Mu.Unlock()
	m.Reply(ctx, msg, "已加载技能："+name)
}

func (m *Middleware) skillUnload(ctx context.Context, msg *wechatbot.IncomingMessage, name string) {
	sess := m.sessions.Current(ctx, msg.UserID)
	sess.Mu.Lock()
	delete(sess.ActiveSkills, name)
	sess.Mu.Unlock()
	m.Reply(ctx, msg, "已卸载技能："+name)
}

func (m *Middleware) sessionList(ctx context.Context, userID string) string {
	metas, current := m.sessions.List(ctx, userID)
	if len(metas) == 0 {
		return "暂无会话，直接发送消息即可开始。"
	}
	var b strings.Builder
	b.WriteString("会话列表：\n")
	for _, meta := range metas {
		mark := "  "
		if meta.ID == current {
			mark = "* "
		}
		fmt.Fprintf(&b, "%s#%s  %s  (%s)\n", mark, meta.ID, meta.Title, meta.CreatedAt.Format("01-02 15:04"))
	}
	b.WriteString("\n切换：/session use <编号>　新建：/session new　删除当前：/session delete")
	return strings.TrimSpace(b.String())
}

func (m *Middleware) sessionNew(ctx context.Context, msg *wechatbot.IncomingMessage) {
	sess := m.sessions.New(ctx, msg.UserID)
	m.Reply(ctx, msg, fmt.Sprintf("已新建会话 #%s 并切换。", sess.ID))
}

// compactCurrent summarizes the current session's older history on demand,
// keeping recent messages verbatim. It serializes against the session lock so a
// concurrent turn never races on history.
func (m *Middleware) compactCurrent(ctx context.Context, msg *wechatbot.IncomingMessage) {
	sess := m.sessions.Current(ctx, msg.UserID)
	sess.Mu.Lock()
	defer sess.Mu.Unlock()
	before, after, err := m.engine.CompactSession(ctx, sess)
	if err != nil {
		m.Reply(ctx, msg, "压缩会话失败："+err.Error())
		return
	}
	if after >= before {
		m.Reply(ctx, msg, "当前会话较短，暂无需压缩。")
		return
	}
	m.Reply(ctx, msg, fmt.Sprintf("已压缩会话：%d 条消息合并为 %d 条。", before, after))
}

func (m *Middleware) sessionUse(ctx context.Context, msg *wechatbot.IncomingMessage, id string) {
	sess, err := m.sessions.Select(ctx, msg.UserID, id)
	if err != nil {
		m.Reply(ctx, msg, "未找到会话 #"+id)
		return
	}
	m.Reply(ctx, msg, fmt.Sprintf("已切换到会话 #%s。", sess.ID))
}

func (m *Middleware) sessionDelete(ctx context.Context, msg *wechatbot.IncomingMessage, id string) {
	sess, err := m.sessions.Delete(ctx, msg.UserID, id)
	if err != nil {
		m.Reply(ctx, msg, "未找到会话 #"+id)
		return
	}
	m.Reply(ctx, msg, fmt.Sprintf("已删除会话 #%s，当前会话为 #%s。", id, sess.ID))
}

func (m *Middleware) sessionDeleteCurrent(ctx context.Context, msg *wechatbot.IncomingMessage) {
	_, current := m.sessions.List(ctx, msg.UserID)
	if current == "" {
		m.Reply(ctx, msg, "当前没有可删除的会话。")
		return
	}
	sess, err := m.sessions.Delete(ctx, msg.UserID, current)
	if err != nil {
		m.Reply(ctx, msg, "删除失败："+err.Error())
		return
	}
	m.Reply(ctx, msg, fmt.Sprintf("已删除会话 #%s，当前会话切换为 #%s。", current, sess.ID))
}

// parseDecision interprets an approval reply. The second return value is false
// if the text is not a recognized decision. "always" approves and asks the
// engine to remember the action's scope; "yes" approves only the current call.
func parseDecision(text string) (Decision, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "always", "always allow", "remember", "始终", "总是", "记住", "永久", "一直":
		return DecisionAlways, true
	case "yes", "y", "ok", "approve", "确认", "是", "好", "同意", "可以":
		return DecisionApprove, true
	case "no", "n", "cancel", "deny", "取消", "否", "不", "不行", "拒绝":
		return DecisionDeny, true
	}
	return DecisionDeny, false
}
