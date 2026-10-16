package benchmark

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/benchmark/browserbench"
	"github.com/hyscale-lab/aries/pkg/config"
)

// NewBrowserBench constructs the benchmark for preparation or execution.
func NewBrowserBench(cfg config.Config, outputRoot string, taskIDs, executionIDs []string) (*browserbench.Benchmark, error) {
	options := browserbench.Options{
		Root:             cfg.Benchmark.Root,
		TaskIDs:          taskIDs,
		ExecutionTaskIDs: executionIDs,
		OutputDir:        outputRoot,
		Dataset:          browserBenchDataset(cfg),
		Environment:      environmentFromConfig(cfg.Benchmark.Environment),
	}
	if settings := cfg.Benchmark.BrowserBench; settings != nil {
		options.MCPPort = settings.MCPPort
		options.MCPCallTimeout = settings.MCPCallTimeoutSeconds
		options.BlockedOrigins = settings.BlockedOrigins
	}
	return browserbench.New(options)
}

// SetupBrowserBench prepares the pinned AssistantBench file.
func SetupBrowserBench(ctx context.Context, cfg config.Config) error {
	return browserbench.Setup(ctx, cfg.Benchmark.Root, browserBenchDataset(cfg), nil)
}

func browserBenchDataset(cfg config.Config) browserbench.Dataset {
	pin := cfg.Versions.BrowserBench.AssistantBench
	return browserbench.Dataset{RepositoryURL: pin.RepositoryURL, Revision: pin.Revision, File: pin.DevFile, SHA256: pin.DevSHA256}
}
