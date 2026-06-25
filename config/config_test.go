package config

import (
	"os"
	"testing"
)

// TestEnvBindsAgentMemory guards the viper AutomaticEnv binding for the memory
// keys: these have no config-file entry in a default checkout, so they only bind
// from the environment because they are registered via SetDefault. If those
// defaults are removed, AutomaticEnv silently stops resolving them.
func TestEnvBindsAgentMemory(t *testing.T) {
	t.Setenv("ZZY_AGENT_MEMORY_ENABLED", "true")
	t.Setenv("ZZY_AGENT_MEMORY_INJECT", "3")
	t.Setenv("ZZY_AGENT_EMBEDDING_MODEL", "text-embedding-3-large")

	// Load reads config.toml from "." if present; run from a temp dir so a
	// developer's local config can't influence the result.
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Agent.MemoryEnabled {
		t.Errorf("MemoryEnabled = false, want true from ZZY_AGENT_MEMORY_ENABLED")
	}
	if cfg.Agent.MemoryInject != 3 {
		t.Errorf("MemoryInject = %d, want 3 from ZZY_AGENT_MEMORY_INJECT", cfg.Agent.MemoryInject)
	}
	if cfg.Agent.EmbeddingModel != "text-embedding-3-large" {
		t.Errorf("EmbeddingModel = %q, want %q", cfg.Agent.EmbeddingModel, "text-embedding-3-large")
	}
}
