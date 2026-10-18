package bridge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/suspend"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
)

type ServeOptions struct {
	Config    LaunchConfig
	Backend   target.Backend
	NewNative func(ssh.Signer, string, string) (NativeServer, error)
}

// Serve owns the run service's host signer and independent native sandbox access.
func Serve(ctx context.Context, options ServeOptions) error { return serve(ctx, options, nil) }
func serve(ctx context.Context, options ServeOptions, listener net.Listener) error {
	if options.Config.RunID == "" || options.Backend == nil || options.NewNative == nil {
		if listener != nil {
			listener.Close()
		}
		return errors.New("bridge requires run identity, backend and native factory")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return err
	}
	var proxy *mcpProxy
	if options.Config.Suspend.Proxied() {
		if proxy, err = startMCPProxy(options.Config.MCPProxyPort); err != nil {
			if listener != nil {
				listener.Close()
			}
			return err
		}
		defer proxy.close()
	}
	service, err := control.NewServer(control.Config{NewSandbox: func(request *v1.RegisterSandboxRequest) control.Sandbox {
		return &nativeSandbox{options: options, request: request, signer: signer, root: filepath.Join(options.Config.OutputDir, request.SandboxId), proxy: proxy}
	}})
	if err != nil {
		return err
	}
	if listener == nil {
		listener, err = net.Listen("tcp", options.Config.ControlAddress)
		if err != nil {
			return err
		}
	}
	defer listener.Close()
	server := grpc.NewServer()
	service.Register(server)
	defer server.Stop()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-done:
		serveErr = fmt.Errorf("bridge control serving stopped: %w", err)
	}
	cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return errors.Join(serveErr, service.Close(cleanup))
}

// This object belongs to one control entry; no second assignment registry exists.
type nativeSandbox struct {
	options  ServeOptions
	request  *v1.RegisterSandboxRequest
	signer   ssh.Signer
	native   NativeServer
	borrowed *target.Borrowed
	root     string
	ready    bool
	proxy    *mcpProxy
	gate     *suspend.Gate
	record   *os.File
}

// suspendRecord is the gate's evidence file, written only in pause mode.
const suspendRecord = "suspend.jsonl"

// startGate counts this sandbox's tool calls and, in pause mode, pauses it
// between them.
func (s *nativeSandbox) startGate() error {
	policy := s.options.Config.Suspend
	options := suspend.Options{Pause: policy.Mode == core.SuspendPause, Grace: time.Duration(policy.GraceMS) * time.Millisecond}
	if options.Pause {
		pause, unpause, ok := s.borrowed.Suspender()
		if !ok {
			return errors.New("bridge execution backend cannot pause sandboxes")
		}
		options.Suspender = funcSuspender{pause, unpause}
		if err := os.MkdirAll(s.root, 0700); err != nil {
			return err
		}
		record, err := os.OpenFile(filepath.Join(s.root, suspendRecord), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		s.record = record
		options.Record = record
	}
	gate, err := suspend.New(options)
	if err != nil {
		return err
	}
	s.gate = gate
	return nil
}

// closeGate resumes the sandbox for evaluation and finalizes the record.
func (s *nativeSandbox) closeGate(ctx context.Context) error {
	var err error
	if s.gate != nil {
		err = s.gate.Close(ctx)
	}
	if s.record != nil {
		err = errors.Join(err, s.record.Close())
		s.record = nil
	}
	return err
}

type funcSuspender struct{ pause, unpause func(context.Context) error }

func (f funcSuspender) Pause(ctx context.Context) error   { return f.pause(ctx) }
func (f funcSuspender) Unpause(ctx context.Context) error { return f.unpause(ctx) }

// gatedExecutor holds the gate open for each command, so a sandbox is never
// paused while a command runs in it.
type gatedExecutor struct {
	target.Executor
	gate *suspend.Gate
}

func (e gatedExecutor) ExecStream(ctx context.Context, c core.Command, in io.Reader, out, errout io.Writer) (core.CommandResult, error) {
	e.gate.Acquire(ctx)
	defer e.gate.Release()
	return e.Executor.ExecStream(ctx, c, in, out, errout)
}

func (s *nativeSandbox) Register(ctx context.Context) (*v1.Endpoint, error) {
	descriptor := control.TargetFromRegistration(s.request, s.options.Config.RunID, s.options.Config.Backend)
	var err error
	s.borrowed, err = target.New(ctx, descriptor, s.options.Backend)
	if err != nil {
		return nil, err
	}
	var executor target.Executor = s.borrowed
	if s.proxy != nil {
		if err = s.startGate(); err != nil {
			return nil, err
		}
		executor = gatedExecutor{Executor: s.borrowed, gate: s.gate}
	}
	s.native, err = s.options.NewNative(s.signer, s.request.SandboxId, s.root)
	if err != nil {
		return nil, err
	}
	endpoint, err := s.native.StartTarget(ctx, executor)
	if err != nil {
		return nil, err
	}
	if s.proxy != nil {
		s.proxy.add(s.request.SandboxId, s.gate)
	}
	_, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return nil, err
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}
	s.ready = true
	return &v1.Endpoint{Port: uint32(number), User: endpoint.Username, Transport: endpoint.Protocol}, nil
}
func (s *nativeSandbox) Release(ctx context.Context) ([]*v1.Artifact, error) {
	if s.proxy != nil {
		s.proxy.remove(s.request.SandboxId)
	}
	if s.borrowed != nil {
		s.borrowed.Revoke()
	}
	if s.native != nil {
		if err := s.native.Stop(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.closeGate(ctx); err != nil {
		return nil, err
	}
	names := EvidenceNames(s.options.Config.RetainRawLog, s.options.Config.Suspend)
	result := make([]*v1.Artifact, 0, len(names))
	for _, name := range names {
		entries, err := finalizeArtifacts(s.root, []string{name})
		if errors.Is(err, os.ErrNotExist) && !s.ready {
			result = append(result, &v1.Artifact{Name: name, Status: "absent"})
			continue
		}
		if err != nil {
			return nil, err
		}
		a := entries[0]
		result = append(result, &v1.Artifact{Name: a.Name, Status: "complete", Size: a.Size, Sha256: a.SHA256})
	}
	// The control entry retains only the immutable request and terminal manifest.
	s.native = nil
	s.borrowed = nil
	s.signer = nil
	return result, nil
}

// EvidenceNames lists the files a released sandbox's bridge entry finalizes.
func EvidenceNames(retainRawLog bool, policy core.SandboxSuspend) []string {
	names := []string{"tool-calls.jsonl"}
	if retainRawLog {
		names = append(names, "ssh_raw.log")
	}
	if policy.Mode == core.SuspendPause {
		names = append(names, suspendRecord)
	}
	return names
}
