package botmgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"zzy/middlewares"

	wechatbot "github.com/corespeed-io/wechatbot/golang"
)

type BotMgrMiddleware struct {
	middlewares.BotClient
	manager *Manager
	router  *middlewares.CommandRouter
}

func NewMiddleware(manager *Manager, bot *wechatbot.Bot) *BotMgrMiddleware {
	m := &BotMgrMiddleware{
		BotClient: middlewares.BotClient{Bot: bot},
		manager:   manager,
	}
	m.router = m.buildRouter()
	return m
}

var _ middlewares.Middleware = (*BotMgrMiddleware)(nil)

func (m *BotMgrMiddleware) OnStart(ctx context.Context) {
	entries, err := os.ReadDir(m.manager.credBaseDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// skip bots that already exist (e.g. master)
		if _, err := m.manager.GetBot(name); err == nil {
			continue
		}
		runtime, err := m.manager.CreateBot(name, false)
		if err != nil {
			continue
		}
		// auto login & start if credentials file exists
		credPath := filepath.Join(m.manager.credBaseDir, name, "credentials.json")
		if _, err := os.Stat(credPath); err == nil {
			if err := m.manager.LoginAndStartAsync(name); err != nil {
				runtime.logf("warn", "auto login failed: %v", err)
			}
		}
	}
}

func (m *BotMgrMiddleware) Name() string {
	return "botmgr"
}

func (m *BotMgrMiddleware) HandleMessage(ctx context.Context, msg *wechatbot.IncomingMessage) bool {
	if msg.Type != wechatbot.ContentText {
		return false
	}
	return m.router.Dispatch(ctx, msg, msg.Text, func(usage string) { m.Reply(ctx, msg, usage) })
}

// buildRouter wires the /bot command table. The hidden "/bot" catch-all is
// listed last so unknown or bare /bot commands show usage instead of falling
// through to other middlewares.
func (m *BotMgrMiddleware) buildRouter() *middlewares.CommandRouter {
	return middlewares.NewCommandRouter(
		middlewares.Command{
			Aliases: []string{"/bot new"},
			ArgHint: "<name>",
			MinArgs: 1,
			Desc:    "新建 bot",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string) {
				if _, err := m.manager.CreateBot(args[0], false); err != nil {
					m.Reply(ctx, msg, fmt.Sprintf("创建 bot 失败: %v", err))
					return
				}
				m.Reply(ctx, msg, fmt.Sprintf("bot %s 已创建", args[0]))
			},
		},
		middlewares.Command{
			Aliases: []string{"/bot del"},
			ArgHint: "<name>",
			MinArgs: 1,
			Desc:    "删除 bot",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string) {
				if err := m.manager.DeleteBot(args[0]); err != nil {
					m.Reply(ctx, msg, fmt.Sprintf("删除 bot 失败: %v", err))
					return
				}
				m.Reply(ctx, msg, fmt.Sprintf("bot %s 已删除", args[0]))
			},
		},
		middlewares.Command{
			Aliases: []string{"/bot list"},
			Desc:    "列出所有 bot",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, _ []string) {
				infos := m.manager.ListBots()
				lines := make([]string, 0, len(infos)+1)
				lines = append(lines, "当前 bots:")
				for _, info := range infos {
					status := "not logged in"
					if info.LoggedIn {
						status = "logged in"
					}
					if info.LoginInProgress {
						status += ", login in progress"
					}
					if info.Running {
						status += ", running"
					}
					prefix := "-"
					if info.IsMaster {
						prefix = "*"
					}
					lines = append(lines, fmt.Sprintf("%s %s: %s", prefix, info.Name, status))
				}
				m.ReplyChunks(ctx, msg, strings.Join(lines, "\n"))
			},
		},
		middlewares.Command{
			Aliases: []string{"/bot login"},
			ArgHint: "<name>",
			MinArgs: 1,
			Desc:    "登录并启动 bot",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string) {
				if err := m.manager.LoginAndStartAsync(args[0]); err != nil {
					m.Reply(ctx, msg, fmt.Sprintf("bot 登录失败: %v", err))
					return
				}
				m.Reply(ctx, msg, fmt.Sprintf("bot %s 开始登录，请使用 /bot log %s 查看二维码和状态", args[0], args[0]))
			},
		},
		middlewares.Command{
			Aliases: []string{"/bot log"},
			ArgHint: "<name>",
			MinArgs: 1,
			Desc:    "查看 bot 日志",
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string) {
				lines, err := m.manager.LastLogLines(args[0], 50)
				if err != nil {
					m.Reply(ctx, msg, fmt.Sprintf("读取 bot 日志失败: %v", err))
					return
				}
				if len(lines) == 0 {
					m.Reply(ctx, msg, fmt.Sprintf("bot %s 暂无日志", args[0]))
					return
				}
				m.ReplyChunks(ctx, msg, strings.Join(lines, "\n"))
			},
		},
		middlewares.Command{
			// Catch-all: claims the whole /bot namespace so unknown or bare /bot
			// commands show usage rather than reaching the agent.
			Aliases: []string{"/bot"},
			Hidden:  true,
			Run: func(ctx context.Context, msg *wechatbot.IncomingMessage, _ []string) {
				m.Reply(ctx, msg, m.router.Help("支持的命令:", ""))
			},
		},
	)
}
