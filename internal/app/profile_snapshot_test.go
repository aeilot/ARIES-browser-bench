package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestProfileSnapshotAllowlist(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	if err := createRunOutputRoot(root); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Model.ID = "model"
	cfg.Model.BaseURL = "https://PRIVATE_SENTINEL"
	cfg.Model.APIKeyEnv = "PRIVATE_SENTINEL"
	cfg.Benchmark.Root = "PRIVATE_SENTINEL"
	cfg.Benchmark.Tasks = []string{"PRIVATE_SENTINEL"}
	cfg.Runtime.Backend = "openai"
	cfg.Execution.Concurrency = 3
	if err := persistProfileSnapshot(root, cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "run-profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "PRIVATE_SENTINEL") {
		t.Fatal("private config leaked")
	}
	if !strings.Contains(string(b), `"concurrency": 3`) {
		t.Fatal("comparison field missing")
	}
	info, err := os.Stat(filepath.Join(root, "run-profile.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("snapshot not private")
	}
}

func TestProfileSnapshotIndependentResourceAndRequestSettings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	if e := createRunOutputRoot(root); e != nil {
		t.Fatal(e)
	}
	harnessCPU, sandboxCPU := 0.0, 4.0
	harnessMemory, sandboxMemory := 2048, 4096
	temperature := 0.0
	cfg := config.Config{}
	cfg.Overrides.HarnessResources = config.ResourceOverrides{CPU: &harnessCPU, MemoryMB: &harnessMemory}
	cfg.Overrides.AgentSandboxResources = config.ResourceOverrides{CPU: &sandboxCPU, MemoryMB: &sandboxMemory}
	cfg.Model.Temperature = &temperature
	cfg.Model.MaxTokens = 512
	cfg.Model.ContextLength = 8192
	cfg.Model.ReasoningEffort = "high"
	if e := persistProfileSnapshot(root, cfg); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(root, "run-profile.json"))
	if e != nil {
		t.Fatal(e)
	}
	var snapshot map[string]any
	if e = json.Unmarshal(b, &snapshot); e != nil {
		t.Fatal(e)
	}
	if snapshot["harness_cpu_limit"] != float64(0) || snapshot["sandbox_cpu_limit"] != float64(4) || snapshot["harness_memory_limit_mb"] != float64(2048) || snapshot["sandbox_memory_limit_mb"] != float64(4096) || snapshot["temperature"] != float64(0) || snapshot["max_tokens"] != float64(512) || snapshot["reasoning_effort"] != "high" {
		t.Fatal(snapshot)
	}
	if _, ok := snapshot["cpu_limit"]; ok {
		t.Fatal("ambiguous resource limit")
	}
	cfg = config.Config{}
	root = filepath.Join(t.TempDir(), "run-defaults")
	if e = createRunOutputRoot(root); e != nil {
		t.Fatal(e)
	}
	if e = persistProfileSnapshot(root, cfg); e != nil {
		t.Fatal(e)
	}
	b, e = os.ReadFile(filepath.Join(root, "run-profile.json"))
	if e != nil {
		t.Fatal(e)
	}
	snapshot = map[string]any{}
	_ = json.Unmarshal(b, &snapshot)
	for _, key := range []string{"harness_cpu_limit", "sandbox_cpu_limit", "temperature", "max_tokens", "context_length", "reasoning_effort"} {
		if _, ok := snapshot[key]; ok {
			t.Fatalf("omitted default manufactured for %s", key)
		}
	}
}

func TestProfileSnapshotSweepConditions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	if e := createRunOutputRoot(root); e != nil {
		t.Fatal(e)
	}
	timeout := 600.0
	cfg := config.Config{}
	cfg.Harness.Type = "hermes"
	cfg.Versions.Hermes.Image = "hermes:v1"
	cfg.Versions.OpenClaw.Image = "openclaw:v1"
	cfg.Overrides.AgentTimeoutSeconds = &timeout
	cfg.Benchmark.Tasks = []string{"a", "b"}
	cfg.Benchmark.Environment = &config.BenchmarkEnvironment{Image: "sandbox:v1", ShmSizeMB: 256, Workdir: "PRIVATE_SENTINEL"}
	if e := persistProfileSnapshot(root, cfg); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(root, "run-profile.json"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "PRIVATE_SENTINEL") {
		t.Fatal("private config leaked")
	}
	var snapshot map[string]any
	if e = json.Unmarshal(b, &snapshot); e != nil {
		t.Fatal(e)
	}
	if snapshot["agent_timeout_seconds"] != float64(600) || snapshot["sandbox_shm_size_mb"] != float64(256) || snapshot["sandbox_image"] != "sandbox:v1" || snapshot["harness_image"] != "hermes:v1" || snapshot["task_count"] != float64(2) {
		t.Fatal(snapshot)
	}
}
