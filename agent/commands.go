package agent

import (
	"context"

	"zzy/middlewares"

	wechatbot "github.com/corespeed-io/wechatbot/golang"
)

const (
	helpHeader = "通用智能助手命令："
	helpFooter = "\n直接发送消息即可与助手对话。当助手需要执行危险操作时，会请求你回复 yes/no 确认。"
)

// dispatchCommand routes a leading-slash control command through the shared
// router. It returns false when the text is not a recognized command so the
// caller can fall back to the conversational agent.
func (m *Middleware) dispatchCommand(ctx context.Context, msg *wechatbot.IncomingMessage, text string) bool {
	return m.router.Dispatch(ctx, msg, text, func(usage string) { m.Reply(ctx, msg, usage) })
}

// commandHelp renders the full help text from the command table so the listing
// always stays in sync with the registered commands.
func (m *Middleware) commandHelp() string {
	return m.router.Help(helpHeader, helpFooter)
}

// buildRouter wires the agent's command table. Handlers are method closures over
// the middleware, so the router can be built once and reused for every message.
func (m *Middleware) buildRouter() *middlewares.CommandRouter {
	return middlewares.NewCommandRouter(
		middlewares.Command{
			Aliases: []string{"/agent help"},
			Desc:    "显示帮助",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, _ []string) {
				m.Reply(ctx, msg, m.commandHelp())
			},
		},
		middlewares.Command{
			Aliases: []string{"/agent reset"},
			Desc:    "清空当前会话记忆",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, _ []string) {
				m.sessions.ClearCurrent(ctx, msg.UserID)
				m.Reply(ctx, msg, "已清空当前会话的记忆。")
			},
		},
		middlewares.Command{
			Aliases: []string{"/session list"},
			Desc:    "查看我的会话列表",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, _ []string) {
				m.Reply(ctx, msg, m.sessionList(ctx, msg.UserID))
			},
		},
		middlewares.Command{
			Aliases: []string{"/session new", "/new", "/clear"},
			Desc:    "新建并切换到新会话",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, _ []string) {
				m.sessionNew(ctx, msg)
			},
		},
		middlewares.Command{
			Aliases: []string{"/session use", "/session select"},
			ArgHint: "<编号>",
			MinArgs: 1,
			Desc:    "切换到指定会话",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string) {
				m.sessionUse(ctx, msg, args[0])
			},
		},
		middlewares.Command{
			// No args deletes the current session; one arg deletes that session.
			Aliases: []string{"/session delete"},
			ArgHint: "[编号]",
			Desc:    "删除会话（缺省删除当前会话）",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string) {
				if len(args) == 0 {
					m.sessionDeleteCurrent(ctx, msg)
					return
				}
				m.sessionDelete(ctx, msg, args[0])
			},
		},
		middlewares.Command{
			Aliases: []string{"/skill list"},
			Desc:    "列出可用技能",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, _ []string) {
				m.Reply(ctx, msg, m.skillList(ctx, msg.UserID))
			},
		},
		middlewares.Command{
			Aliases: []string{"/skill reload"},
			Desc:    "从磁盘重新扫描技能",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, _ []string) {
				if err := m.skills.Reload(msg.UserID); err != nil {
					m.Reply(ctx, msg, "重新加载技能失败："+err.Error())
					return
				}
				m.Reply(ctx, msg, "已从磁盘重新加载技能。")
			},
		},
		middlewares.Command{
			Aliases: []string{"/skill load"},
			ArgHint: "<技能名>",
			MinArgs: 1,
			Desc:    "加载技能",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string) {
				m.skillLoad(ctx, msg, args[0])
			},
		},
		middlewares.Command{
			Aliases: []string{"/skill unload"},
			ArgHint: "<技能名>",
			MinArgs: 1,
			Desc:    "卸载技能",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string) {
				m.skillUnload(ctx, msg, args[0])
			},
		},
	)
}
