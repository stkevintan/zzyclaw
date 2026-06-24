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
	engine, sessions, skillMgr, err := buildAgent(ctx, cfg, githubToken)
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
				agent.NewMiddleware(bot, engine, sessions, skillMgr),
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
// store, the disk-backed skill manager (shared builtins seeded with the
// write-skill skill plus per-user skill directories), the built-in tools
// (sandboxed filesystem + script runner + skill management) and the ReAct
// engine.
func buildAgent(ctx context.Context, cfg *config.Config, githubToken string) (*agent.Engine, *agent.SessionManager, *skill.Manager, error) {
	agentBase := filepath.Join(cfg.DataDir, "agent")
	skillsDir := orDefault(cfg.Agent.SkillsDir, filepath.Join(agentBase, "skills"))
	workspaceDir := orDefault(cfg.Agent.WorkspaceDir, filepath.Join(agentBase, "workspace"))
	for _, d := range []string{skillsDir, workspaceDir} {
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

	// Skills: shared builtins live in skillsDir (seeded), while each user's own
	// skills live under their private workspace subdirectory. The userDir closure
	// keeps the skill package agnostic of the workspace layout and reuses the same
	// per-user isolation as the filesystem sandbox.
	skillMgr, err := skill.NewManager(skillsDir, func(userID string) (string, error) {
		ws, err := tools.UserWorkspace(workspaceDir, userID)
		if err != nil {
			return "", err
		}
		return filepath.Join(ws, "skills"), nil
	})
	if err != nil {
		return nil, nil, nil, err
	}

	sandbox, err := tools.NewSandbox(workspaceDir, skillsDir)
	if err != nil {
		return nil, nil, nil, err
	}
	toolReg := tools.NewRegistry()
	toolReg.Register(tools.NewReadFile(sandbox))
	toolReg.Register(tools.NewWriteFile(sandbox))
	toolReg.Register(tools.NewEditFile(sandbox))
	toolReg.Register(tools.NewListDir(sandbox))
	toolReg.Register(tools.NewDeletePath(sandbox))
	toolReg.Register(tools.NewSearchFiles(sandbox, 30*time.Second))
	toolReg.Register(tools.NewShell(sandbox, time.Duration(cfg.Agent.ShellTimeoutSeconds)*time.Second))
	toolReg.Register(tools.NewHTTPGet(cfg.Agent.NetworkAllowlist, 30*time.Second))

	// Sandboxed skills: their code runs inside the Deno sandbox. By default a
	// skill gets read-only access to its own directory and the workspace and no
	// network; skills may opt into workspace writes or specific network hosts.
	denoCacheDir := filepath.Join(agentBase, "deno-cache")
	denoRunner := tools.NewDenoRunner(cfg.Agent.DenoPath, denoCacheDir, time.Duration(cfg.Agent.SkillTimeoutSeconds)*time.Second)
	toolReg.Register(agent.RunSkillTool(skillMgr, denoRunner, workspaceDir))
	if denoRunner.Installed() {
		slog.Info("deno skills enabled", "deno", denoRunner.Path())
	} else {
		slog.Info("deno skills inactive: deno not found (install Deno or set agent.deno_path)")
	}

	for _, t := range agent.SkillTools(skillMgr) {
		toolReg.Register(t)
	}

	agentModel := orDefault(cfg.Agent.Model, cfg.Copilot.Model)
	agentClient := copilot.NewClient(githubToken, copilot.WithModel(agentModel))

	// Persistent, per-user access-control store: remembers each user's "always
	// approve" decisions (e.g. an http_get host or a workspace directory) so the
	// agent doesn't re-prompt for the same scope. It reuses the shared store, so
	// grants get the same durability as conversation memory and session indexes
	// (persistent with Redis, process-local otherwise).
	grants := agent.NewStoreGrantStore(store)

	engine := agent.NewEngine(agentClient, toolReg, skillMgr, store, agent.EngineConfig{
		MaxIterations: cfg.Agent.MaxIterations,
		MaxHistory:    cfg.Agent.MaxHistory,
		AutoApprove:   cfg.Agent.AutoApprove,
		Owners:        cfg.Agent.Owners,
		Grants:        grants,
	})
	sessions := agent.NewSessionManager(store)
	return engine, sessions, skillMgr, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
