package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

type targetExporter interface {
	ExportBridgeTarget() (core.BridgeTarget, error)
}

// Session is one sandbox's immutable bridge access and evidence owner.
type Session struct {
	mu                                       sync.Mutex
	service                                  *Service
	outputRoot                               string
	started, stopped                         bool
	descriptor                               core.BridgeTarget
	local, remote, helper                    string
	registered, exposed, released, collected bool
	manifest                                 []*v1.Artifact
	evidenceErr                              error
}

var _ runner.ToolBridge = (*Session)(nil)

func (s *Session) record() error {
	b, err := json.MarshalIndent(struct{ Backend, RuntimeID, RuntimeName, SandboxID, TargetRuntimeID string }{s.service.options.Launch.RuntimeBackend, s.service.runtimeID, s.service.runtimeName, s.descriptor.SandboxID, s.descriptor.RuntimeID}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.local, "runtime.json"), b, 0600)
}
func (s *Session) Start(ctx context.Context, sandbox runner.Sandbox) (core.ToolEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var endpoint core.ToolEndpoint
	if s.started || s.stopped {
		return endpoint, errors.New("bridge session cannot be reused")
	}
	s.started = true
	s.service.mu.Lock()
	running := s.service.state == "running"
	s.service.mu.Unlock()
	if !running {
		return endpoint, errors.New("bridge service is not running")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	exporter, ok := sandbox.(targetExporter)
	if !ok {
		return endpoint, errors.New("sandbox cannot export a bridge target")
	}
	d, err := exporter.ExportBridgeTarget()
	if err != nil {
		return endpoint, err
	}
	if err = target.Validate(d); err != nil {
		return endpoint, err
	}
	if d.Backend != s.service.options.Launch.Config.Backend || d.RunID != s.service.options.RunID {
		return endpoint, errors.New("sandbox backend or run does not match bridge service")
	}
	s.descriptor = d
	s.local = filepath.Join(s.outputRoot, d.TaskID, "bridge")
	s.remote = filepath.Join(s.service.options.Launch.Config.OutputDir, d.SandboxID)
	if err = os.MkdirAll(s.local, 0700); err != nil {
		return endpoint, err
	}
	if err = s.record(); err != nil {
		return endpoint, err
	}
	s.registered = true // Retain cleanup ownership even if the response is lost.
	access, err := s.registerSandbox(ctx, control.Registration(d))
	if err != nil {
		return endpoint, err
	}
	if access.State != v1.State_READY || access.Endpoint == nil {
		return endpoint, fmt.Errorf("bridge sandbox registration failed: state %s: %s", access.State, access.Error)
	}
	native := access.Endpoint
	if native.Transport != "ssh" || native.User != "aries" || native.Port == 0 || native.Port > 65535 {
		return endpoint, errors.New("invalid native bridge endpoint")
	}
	// Binding is reported by the child; deployment resolves the consumer address.
	address, err := s.service.options.Runtime.(deployment.ServiceRuntime).TaskAddress(ctx, s.service.runtimeID, int(native.Port))
	if err != nil {
		return endpoint, err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return endpoint, errors.New("invalid resolved native endpoint")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return endpoint, errors.New("invalid resolved native endpoint port")
	}
	endpoint = core.ToolEndpoint{Protocol: "ssh", Address: address, Username: native.User, Workdir: d.Workdir, LogPaths: []string{filepath.Join(s.local, "tool-calls.jsonl")}}
	if s.service.options.RetainRawLog {
		endpoint.LogPaths = append(endpoint.LogPaths, filepath.Join(s.local, "ssh_raw.log"))
	}
	if s.service.options.Launch.Config.Suspend.Proxied() {
		if endpoint.MCPServers, err = s.proxiedMCPServers(ctx, sandbox.Connectivity().MCPServers); err != nil {
			return core.ToolEndpoint{}, err
		}
	}
	if s.service.options.Launch.Config.Suspend.Mode == core.SuspendPause {
		endpoint.LogPaths = append(endpoint.LogPaths, filepath.Join(s.local, suspendRecord))
	}
	client := s.service.options.Client
	if client.SourcePath != "" {
		endpoint.ClientCommand = client.Command
		endpoint.ClientSourceFile = filepath.Join(s.local, filepath.Base(client.Command))
		s.helper = endpoint.ClientSourceFile
		if err = stageClientHelper(client.SourcePath, s.helper); err != nil {
			return core.ToolEndpoint{}, err
		}
	}
	s.exposed = true
	return endpoint, nil
}

// proxiedMCPServers points each of the sandbox's MCP servers at the bridge's
// proxy, which forwards to the same server and counts its calls.
func (s *Session) proxiedMCPServers(ctx context.Context, servers []core.MCPServerConfig) ([]core.MCPServerConfig, error) {
	if len(servers) == 0 {
		return nil, nil
	}
	proxyPort := s.service.options.Launch.Config.MCPProxyPort
	address, err := s.service.options.Runtime.(deployment.ServiceRuntime).TaskAddress(ctx, s.service.runtimeID, proxyPort)
	if err != nil {
		return nil, fmt.Errorf("resolve bridge MCP proxy: %w", err)
	}
	out := make([]core.MCPServerConfig, 0, len(servers))
	for _, server := range servers {
		u, err := url.Parse(server.URL)
		if err != nil || u.Scheme != "http" || u.Hostname() != s.descriptor.SandboxID || u.Port() == "" {
			return nil, fmt.Errorf("MCP server %q is not served by this sandbox over HTTP", server.Name)
		}
		proxied := server
		proxied.URL = (&url.URL{Scheme: "http", Host: address, Path: MCPProxyPath + s.descriptor.SandboxID + "/" + u.Port() + u.EscapedPath(), RawQuery: u.RawQuery}).String()
		out = append(out, proxied)
	}
	return out, nil
}
func (s *Session) request() *v1.SandboxRequest {
	return &v1.SandboxRequest{SandboxId: s.descriptor.SandboxID}
}
func (s *Session) validateAccess(a *v1.SandboxAccess) error {
	if a == nil || a.SandboxId != s.descriptor.SandboxID {
		return errors.New("bridge sandbox identity differs")
	}
	return nil
}
func (s *Session) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	if s.evidenceErr != nil {
		return errors.Join(s.evidenceErr, s.removeHelper())
	}
	if s.registered && !s.released {
		a, err := s.service.client.ReleaseSandbox(ctx, s.request())
		if err != nil {
			running, runErr := s.service.options.Runtime.Running(ctx, s.service.runtimeID)
			if runErr != nil || running {
				return errors.Join(fmt.Errorf("bridge release unconfirmed: %w", err), runErr)
			}
			s.released = true
			if s.exposed {
				s.evidenceErr = errors.New("bridge exited without finalized evidence")
				return errors.Join(s.evidenceErr, s.removeHelper())
			}
			s.collected = true
		} else {
			if err = s.validateAccess(a); err != nil {
				return err
			}
			if a.State != v1.State_RELEASED || a.CleanupError != "" {
				return fmt.Errorf("bridge access closure or evidence finalization unconfirmed (state %s): %s", a.State, a.CleanupError)
			}
			s.released = true
			s.manifest = a.Artifacts
			if !s.exposed && len(a.Artifacts) == 0 {
				s.collected = true
			}
		}
	}
	if s.released && !s.collected {
		expected := map[string]bool{}
		for _, name := range EvidenceNames(s.service.options.RetainRawLog, s.service.options.Launch.Config.Suspend) {
			expected[name] = false
		}
		for _, entry := range s.manifest {
			seen, ok := expected[entry.Name]
			if !ok || seen {
				return errors.New("bridge evidence manifest is incomplete or unexpected")
			}
			if entry.Status == "absent" && !s.exposed && entry.Size == 0 && entry.Sha256 == "" {
				expected[entry.Name] = true
				continue
			}
			if entry.Status != "complete" {
				return errors.New("bridge evidence manifest is incomplete or unexpected")
			}
			if err := collectArtifact(ctx, s.service.options.Runtime, s.service.runtimeID, s.remote, s.local, Artifact{Name: entry.Name, Size: entry.Size, SHA256: entry.Sha256}); err != nil {
				return err
			}
			expected[entry.Name] = true
		}
		for _, seen := range expected {
			if !seen {
				return errors.New("bridge evidence missing from finalized manifest")
			}
		}
		s.collected = true
	}
	return s.removeHelper()
}
func (s *Session) removeHelper() error {
	if s.helper != "" {
		if err := os.Remove(s.helper); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
func stageClientHelper(source, destination string) error {
	const maxHelperBytes int64 = 64 << 20
	before, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0111 == 0 || before.Size() <= 0 || before.Size() > maxHelperBytes {
		return errors.New("bridge helper must be a bounded regular executable")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	content, readErr := io.ReadAll(io.LimitReader(input, maxHelperBytes+1))
	closeErr := input.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		return err
	}
	after, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != int64(len(content)) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("bridge helper source changed while staging")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := output.Write(content)
	modeErr := output.Chmod(0555)
	return errors.Join(writeErr, modeErr, output.Close())
}
