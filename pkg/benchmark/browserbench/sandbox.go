package browserbench

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// mcpStartScript backgrounds the server given as arguments from inside
// browserDir, with its output in browserDir/mcp.log. ARIES replaces the
// image's entrypoint with `sleep infinity`, so nothing in the image starts
// the server; once this shell exits the server is reparented to the
// container's init.
const mcpStartScript = `mkdir -p "$1" && cd "$1" && shift && nohup "$@" >mcp.log 2>&1 &`

// mcpReadyScript succeeds once the server accepts TCP connections.
const mcpReadyScript = `exec 3<>"/dev/tcp/127.0.0.1/$1"`

// Package-level vars so tests can shorten the readiness bound.
var (
	mcpReadyAttempts = 60
	mcpReadyDelay    = 500 * time.Millisecond
)

// PrepareSandbox clears the answer path, the one channel Evaluate trusts,
// then starts the Playwright MCP server and waits until it listens. The gold
// answers never enter the sandbox.
func (b *Benchmark) PrepareSandbox(ctx context.Context, _ core.Task, sandbox runner.Sandbox) error {
	if sandbox == nil {
		return errors.New("browserbench preparation requires a live sandbox")
	}
	removed, err := sandbox.Exec(ctx, core.Command{Path: "/bin/rm", Args: []string{"-rf", "--", answerPath, browserDir}})
	if err != nil {
		return fmt.Errorf("remove answer path before harness: %w", err)
	}
	if removed.ExitCode != 0 {
		return fmt.Errorf("remove answer path before harness: exit code %d", removed.ExitCode)
	}
	const absencePredicate = `for path do [ ! -e "$path" ] && [ ! -L "$path" ] || exit 1; done`
	probed, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", absencePredicate, "aries-answer-absence", answerPath}})
	if err != nil {
		return fmt.Errorf("confirm answer path absent before harness: %w", err)
	}
	if probed.ExitCode != 0 {
		return fmt.Errorf("confirm answer path absent before harness: exit code %d", probed.ExitCode)
	}
	return b.startMCP(ctx, sandbox)
}

// mcpCommand is the server's argv: headless Chromium without its own
// sandbox (the image's default; the container is the boundary), a profile
// kept in memory, and any host name accepted, since the harness addresses
// the sandbox by a runtime name chosen at start.
func (b *Benchmark) mcpCommand() []string {
	command := []string{
		"node", mcpEntrypoint, "--headless", "--browser", "chromium", "--no-sandbox", "--isolated",
		"--host", "0.0.0.0", "--port", strconv.Itoa(b.mcpPort), "--allowed-hosts", "*",
		"--output-dir", browserDir,
	}
	if len(b.blockedOrigins) > 0 {
		command = append(command, "--blocked-origins", strings.Join(b.blockedOrigins, ";"))
	}
	return command
}

func (b *Benchmark) startMCP(ctx context.Context, sandbox runner.Sandbox) error {
	args := append([]string{"-c", mcpStartScript, "aries-browser-start", browserDir}, b.mcpCommand()...)
	started, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: args})
	if err != nil {
		return fmt.Errorf("start Playwright MCP: %w", err)
	}
	if started.ExitCode != 0 {
		return fmt.Errorf("start Playwright MCP: exit code %d: %s", started.ExitCode, strings.TrimSpace(started.Stderr))
	}
	for attempt := 0; attempt < mcpReadyAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("await Playwright MCP readiness: %w", ctx.Err())
			case <-time.After(mcpReadyDelay):
			}
		}
		probed, err := sandbox.Exec(ctx, core.Command{Path: "/bin/bash", Args: []string{"-c", mcpReadyScript, "aries-browser-ready", strconv.Itoa(b.mcpPort)}})
		if err == nil && probed.ExitCode == 0 {
			return nil
		}
	}
	return fmt.Errorf("Playwright MCP did not listen on port %d before the bound%s", b.mcpPort, mcpLogTail(ctx, sandbox))
}

// mcpLogTail returns the end of the server log for an error message.
func mcpLogTail(ctx context.Context, sandbox runner.Sandbox) string {
	tail, err := sandbox.Exec(ctx, core.Command{Path: "/usr/bin/tail", Args: []string{"-n", "20", browserDir + "/mcp.log"}})
	if err != nil || tail.ExitCode != 0 || strings.TrimSpace(tail.Stdout) == "" {
		return ""
	}
	return "; server log ends:\n" + strings.TrimSpace(tail.Stdout)
}
