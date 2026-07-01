package config

import (
	"slices"
	"testing"
)

// loadFromEmptyDir loads config from an isolated empty dir so a developer's local
// config.toml can't influence env-binding assertions, and without the
// process-wide side effects of os.Chdir (keeping these tests parallel-safe).
func loadFromEmptyDir(t *testing.T) *Config {
	t.Helper()
	cfg, err := loadFrom(t.TempDir())
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	return cfg
}

// TestEnvBindsAgentScalars guards that scalar agent keys bind from ZZY_AGENT_*.
// These have no config-file entry in a default checkout, so they only resolve
// because they are registered via SetDefault; AutomaticEnv silently stops
// resolving them if those registrations are removed.
func TestEnvBindsAgentScalars(t *testing.T) {
	t.Setenv("ZZY_AGENT_MEMORY_INJECT", "3")
	t.Setenv("ZZY_AGENT_EMBEDDING_MODEL", "text-embedding-3-large")
	t.Setenv("ZZY_AGENT_MODEL", "gpt-5")
	t.Setenv("ZZY_AGENT_MAX_ITERATIONS", "7")
	t.Setenv("ZZY_AGENT_SHELL_TIMEOUT_SECONDS", "45")
	t.Setenv("ZZY_AGENT_SKILL_MEMORY_MB", "512")

	cfg := loadFromEmptyDir(t)

	if cfg.Agent.MemoryInject != 3 {
		t.Errorf("MemoryInject = %d, want 3", cfg.Agent.MemoryInject)
	}
	if cfg.Agent.EmbeddingModel != "text-embedding-3-large" {
		t.Errorf("EmbeddingModel = %q, want text-embedding-3-large", cfg.Agent.EmbeddingModel)
	}
	if cfg.Agent.Model != "gpt-5" {
		t.Errorf("Model = %q, want gpt-5", cfg.Agent.Model)
	}
	if cfg.Agent.MaxIterations != 7 {
		t.Errorf("MaxIterations = %d, want 7", cfg.Agent.MaxIterations)
	}
	if cfg.Agent.ShellTimeoutSeconds != 45 {
		t.Errorf("ShellTimeoutSeconds = %d, want 45", cfg.Agent.ShellTimeoutSeconds)
	}
	if cfg.Agent.SkillMemoryMB != 512 {
		t.Errorf("SkillMemoryMB = %d, want 512", cfg.Agent.SkillMemoryMB)
	}
}

// TestEnvBindsAgentSlices guards that the []string agent keys bind from env as
// comma-separated lists via viper's default StringToSlice decode hook.
func TestEnvBindsAgentSlices(t *testing.T) {
	t.Setenv("ZZY_AGENT_AUTO_APPROVE", "fetch,run_shell")
	t.Setenv("ZZY_AGENT_OWNERS", "alice,bob")
	t.Setenv("ZZY_AGENT_NETWORK_ALLOWLIST", "example.com,*.github.com")

	cfg := loadFromEmptyDir(t)

	wantEq := func(name string, got, want []string) {
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	wantEq("AutoApprove", cfg.Agent.AutoApprove, []string{"fetch", "run_shell"})
	wantEq("Owners", cfg.Agent.Owners, []string{"alice", "bob"})
	wantEq("NetworkAllowlist", cfg.Agent.NetworkAllowlist, []string{"example.com", "*.github.com"})
}
