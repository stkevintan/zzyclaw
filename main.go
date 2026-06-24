package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"
	"zzy/agent"
	"zzy/agent/skill"
	"zzy/agent/tools"
	"zzy/botmgr"
	"zzy/config"
	"zzy/copilot"
	"zzy/middlewares"
	"zzy/resume"

	wechatbot "github.com/corespeed-io/wechatbot/golang"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.Log.SlogLevel(),
	})))

	ctx := context.Background()

	githubToken, err := copilot.Login(cfg.DataDir)
	if err != nil {
		slog.Error("copilot login failed", "error", err)
		os.Exit(1)
	}

	copilotClient := copilot.NewClient(
		githubToken,
		copilot.WithModel(cfg.Copilot.Model),
	)

	// Assemble the general agent: memory store, skills, tools and the engine.
	engine, sessions, skillReg, err := buildAgent(ctx, cfg, githubToken)
	if err != nil {
		slog.Error("failed to build agent", "error", err)
		os.Exit(1)
	}

	manager := botmgr.NewManager(
		ctx,
		cfg.Log.Level,
		filepath.Join(cfg.DataDir, "bots"),
		func(bot *wechatbot.Bot, locker *middlewares.Locker) []middlewares.Middleware {
			return []middlewares.Middleware{
				&middlewares.LoggingMiddleware{},
				resume.NewMiddleware(bot, copilotClient, locker),
				agent.NewMiddleware(bot, engine, sessions, skillReg),
			}
		},
	)

	masterBot, err := manager.CreateBot("master", true)
	if err != nil {
		slog.Error("failed to create master bot", "error", err)
		os.Exit(1)
	}
	masterBot.AddMiddleware(botmgr.NewMiddleware(manager, masterBot.Bot()))

	creds, err := masterBot.Login(ctx, false)
	if err != nil {
		slog.Error("master bot login failed", "error", err)
		os.Exit(1)
	}
	slog.Info("master bot logged in", "account_id", creds.AccountID)

	if err := masterBot.Start(ctx); err != nil {
		slog.Error("bot stopped", "error", err)
		os.Exit(1)
	}
}

// buildAgent constructs the agent's shared components: the conversation-memory
// store, the disk-backed skill registry (seeded with the write-skill skill), the
// built-in tools (sandboxed filesystem + script runner + skill management) and
// the ReAct engine.
func buildAgent(ctx context.Context, cfg *config.Config, githubToken string) (*agent.Engine, *agent.SessionManager, *skill.Registry, error) {
	agentBase := filepath.Join(cfg.DataDir, "agent")
	skillsDir := orDefault(cfg.Agent.SkillsDir, filepath.Join(agentBase, "skills"))
	scriptsDir := orDefault(cfg.Agent.ScriptsDir, filepath.Join(agentBase, "scripts"))
	workspaceDir := orDefault(cfg.Agent.WorkspaceDir, filepath.Join(agentBase, "workspace"))
	for _, d := range []string{skillsDir, scriptsDir, workspaceDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, nil, nil, err
		}
	}

	// Conversation memory: Redis when configured, otherwise in-memory.
	var store agent.Store
	if cfg.Redis.Addr != "" {
		rs, err := agent.NewRedisStore(ctx, cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB,
			time.Duration(cfg.Redis.TTLSeconds)*time.Second)
		if err != nil {
			return nil, nil, nil, err
		}
		store = rs
		slog.Info("agent memory: redis", "addr", cfg.Redis.Addr)
	} else {
		store = agent.NewInMemoryStore()
		slog.Info("agent memory: in-memory")
	}

	skillReg, err := skill.NewRegistry(skillsDir)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := skillReg.Seed(scriptsDir); err != nil {
		slog.Warn("failed to seed default skills", "error", err)
	}

	sandbox, err := tools.NewSandbox(workspaceDir, skillsDir, scriptsDir)
	if err != nil {
		return nil, nil, nil, err
	}
	toolReg := tools.NewRegistry()
	toolReg.Register(tools.NewReadFile(sandbox))
	toolReg.Register(tools.NewWriteFile(sandbox))
	toolReg.Register(tools.NewListDir(sandbox))
	toolReg.Register(tools.NewDeletePath(sandbox))
	toolReg.Register(tools.NewSearchFiles(sandbox, 30*time.Second))
	toolReg.Register(tools.NewRunScript(scriptsDir, 60*time.Second))
	toolReg.Register(tools.NewPipInstall(180 * time.Second))
	for _, t := range agent.SkillTools(skillReg) {
		toolReg.Register(t)
	}

	agentModel := orDefault(cfg.Agent.Model, cfg.Copilot.Model)
	agentClient := copilot.NewClient(githubToken, copilot.WithModel(agentModel))

	engine := agent.NewEngine(agentClient, toolReg, skillReg, store, agent.EngineConfig{
		MaxIterations: cfg.Agent.MaxIterations,
		MaxHistory:    cfg.Agent.MaxHistory,
		AutoApprove:   cfg.Agent.AutoApprove,
	})
	sessions := agent.NewSessionManager(store)
	return engine, sessions, skillReg, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
