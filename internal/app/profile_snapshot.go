package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/config"
)

// persistProfileSnapshot writes only comparison metadata. Keep this allowlist
// independent from Config marshaling: endpoint URLs, credentials, file paths,
// task instructions and native harness settings never belong in this artifact.
func persistProfileSnapshot(root string, cfg config.Config) error {
	snapshot := map[string]any{
		"benchmark":   map[string]string{"type": cfg.Benchmark.Type},
		"harness":     map[string]string{"type": cfg.Harness.Type},
		"model":       map[string]string{"id": cfg.Model.ID, "provider": cfg.Runtime.Backend},
		"concurrency": cfg.Execution.Concurrency,
	}
	for _, resources := range []struct {
		prefix string
		value  config.ResourceOverrides
	}{{"harness", cfg.Overrides.HarnessResources}, {"sandbox", cfg.Overrides.AgentSandboxResources}} {
		if cpu := resources.value.CPU; cpu != nil {
			snapshot[resources.prefix+"_cpu_limit"] = *cpu
		}
		if memory := resources.value.MemoryMB; memory != nil {
			snapshot[resources.prefix+"_memory_limit_mb"] = *memory
		}
	}
	if timeout := cfg.Overrides.AgentTimeoutSeconds; timeout != nil {
		snapshot["agent_timeout_seconds"] = *timeout
	}
	if environment := cfg.Benchmark.Environment; environment != nil {
		if environment.Image != "" {
			snapshot["sandbox_image"] = environment.Image
		}
		if environment.ShmSizeMB != 0 {
			snapshot["sandbox_shm_size_mb"] = environment.ShmSizeMB
		}
	}
	if image := harnessImage(cfg); image != "" {
		snapshot["harness_image"] = image
	}
	if len(cfg.Benchmark.Tasks) != 0 {
		snapshot["task_count"] = len(cfg.Benchmark.Tasks)
	}
	if cfg.Model.Temperature != nil {
		snapshot["temperature"] = *cfg.Model.Temperature
	}
	if cfg.Model.MaxTokens != 0 {
		snapshot["max_tokens"] = cfg.Model.MaxTokens
	}
	if cfg.Model.ContextLength != 0 {
		snapshot["context_length"] = cfg.Model.ContextLength
	}
	if cfg.Model.ReasoningEffort != "" {
		snapshot["reasoning_effort"] = cfg.Model.ReasoningEffort
	}
	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run profile snapshot: %w", err)
	}
	if err = persistRunResult(filepath.Join(root, "run-profile.json"), append(b, '\n')); err != nil {
		return fmt.Errorf("persist run profile snapshot: %w", err)
	}
	return nil
}

// harnessImage returns the pinned image of the selected harness, which
// identifies its version; harnesses without a pinned image return "".
func harnessImage(cfg config.Config) string {
	switch cfg.Harness.Type {
	case "hermes":
		return cfg.Versions.Hermes.Image
	case "openclaw":
		return cfg.Versions.OpenClaw.Image
	}
	return ""
}
