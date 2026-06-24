package config

import (
	"log/slog"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	DataDir string        `mapstructure:"data_dir"`
	Log     LogConfig     `mapstructure:"log"`
	Copilot CopilotConfig `mapstructure:"copilot"`
	Redis   RedisConfig   `mapstructure:"redis"`
	Agent   AgentConfig   `mapstructure:"agent"`
}

type LogConfig struct {
	Level string `mapstructure:"level"`
}

type CopilotConfig struct {
	Model string `mapstructure:"model"`
}

// RedisConfig configures the optional Redis-backed conversation memory. When
// Addr is empty, an in-memory store is used instead.
type RedisConfig struct {
	Addr       string `mapstructure:"addr"`
	Password   string `mapstructure:"password"`
	DB         int    `mapstructure:"db"`
	TTLSeconds int    `mapstructure:"ttl_seconds"`
}

// AgentConfig configures the general ReAct agent.
type AgentConfig struct {
	Model            string   `mapstructure:"model"`             // overrides copilot.model for the agent when set
	MaxIterations    int      `mapstructure:"max_iterations"`    // max ReAct steps per turn
	MaxHistory       int      `mapstructure:"max_history"`       // max stored messages per session
	CompactThreshold int      `mapstructure:"compact_threshold"` // stored-history length past which older messages are summarized; 0 disables
	CompactKeep      int      `mapstructure:"compact_keep"`      // most-recent messages kept verbatim when compacting
	SkillsDir        string   `mapstructure:"skills_dir"`        // optional shared dir for operator-provided skills (builtins are compiled in); defaults to <data_dir>/agent/skills
	WorkspaceDir     string   `mapstructure:"workspace_dir"`     // base dir for per-user workspaces; defaults to <data_dir>/agent/workspace
	AutoApprove      []string `mapstructure:"auto_approve"`      // tool names that skip the approval prompt
	Owners           []string `mapstructure:"owners"`            // user IDs allowed to run dangerous tools; empty disables the gate

	ShellTimeoutSeconds int `mapstructure:"shell_timeout_seconds"` // max wall-clock per run_shell command

	NetworkAllowlist []string `mapstructure:"network_allowlist"` // pre-trusted domains the http_get tool reaches without prompting (host or *.host); other hosts require per-host approval

	DenoPath            string `mapstructure:"deno_path"`             // path to the Deno binary for sandboxed skills; empty => look up "deno" on PATH
	SkillTimeoutSeconds int    `mapstructure:"skill_timeout_seconds"` // max wall-clock per sandboxed run_skill execution
	SkillMemoryMB       int    `mapstructure:"skill_memory_mb"`       // V8 heap cap (--max-old-space-size) per sandboxed run_skill execution; 0 leaves Deno's default
}

// SlogLevel converts the configured log level string to slog.Level.
func (c *LogConfig) SlogLevel() slog.Level {
	switch strings.ToLower(c.Level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Load reads configuration from file and environment variables.
// It looks for config.toml in the current directory.
func Load() (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("data_dir", "data")
	v.SetDefault("log.level", "info")
	v.SetDefault("copilot.model", "gpt-4o")
	v.SetDefault("redis.addr", "")
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.ttl_seconds", 0)
	v.SetDefault("agent.model", "")
	v.SetDefault("agent.max_iterations", 12)
	v.SetDefault("agent.max_history", 40)
	v.SetDefault("agent.compact_threshold", 30)
	v.SetDefault("agent.compact_keep", 12)
	v.SetDefault("agent.skills_dir", "")
	v.SetDefault("agent.workspace_dir", "")
	v.SetDefault("agent.shell_timeout_seconds", 120)
	v.SetDefault("agent.deno_path", "")
	v.SetDefault("agent.skill_timeout_seconds", 30)
	v.SetDefault("agent.skill_memory_mb", 256)
	// Config file
	v.SetConfigName("config")
	v.SetConfigType("toml")
	v.AddConfigPath(".")
	v.AddConfigPath("/")

	// Environment variables: ZZY_LOG_LEVEL, ZZY_COPILOT_TOKEN, etc.
	v.SetEnvPrefix("ZZY")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
