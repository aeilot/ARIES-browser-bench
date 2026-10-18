package bridge

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/suspend"
	"github.com/hyscale-lab/aries/pkg/core"
)

type countingSuspender struct {
	mu              sync.Mutex
	pauses, resumes int
	paused          chan struct{}
}

func (c *countingSuspender) Pause(context.Context) error {
	c.mu.Lock()
	c.pauses++
	c.mu.Unlock()
	c.paused <- struct{}{}
	return nil
}
func (c *countingSuspender) Unpause(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resumes++
	return nil
}
func (c *countingSuspender) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pauses, c.resumes
}

// testProxy routes every sandbox address to upstream.
func testProxy(t *testing.T, upstream *httptest.Server) (*mcpProxy, *httptest.Server) {
	t.Helper()
	address := upstream.Listener.Addr().String()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	p := &mcpProxy{gates: make(map[string]*suspend.Gate), transport: transport}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return p, front
}

func TestParseMCPProxyPath(t *testing.T) {
	id, port, rest, ok := parseMCPProxyPath("/s/aries-task-1/8931/mcp")
	if !ok || id != "aries-task-1" || port != 8931 || rest != "/mcp" {
		t.Fatalf("%q %d %q %v", id, port, rest, ok)
	}
	if _, _, rest, ok = parseMCPProxyPath("/s/aries-task-1/8931"); !ok || rest != "/" {
		t.Fatalf("missing path must forward to the root, got %q %v", rest, ok)
	}
	for _, bad := range []string{"/mcp", "/s/", "/s/x", "/s/../8931/mcp", "/s/x/0/mcp", "/s/x/70000/mcp", "/s/x/port/mcp"} {
		if _, _, _, ok := parseMCPProxyPath(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestMCPProxyForwardsAndGatesCalls(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.Host+" "+r.URL.RequestURI())
		mu.Unlock()
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	p, front := testProxy(t, upstream)
	s := &countingSuspender{paused: make(chan struct{}, 8)}
	gate, err := suspend.New(suspend.Options{Suspender: s, Pause: true})
	if err != nil {
		t.Fatal(err)
	}
	p.add("sandbox-a", gate)

	post := func() {
		response, err := http.Post(front.URL+"/s/sandbox-a/8931/mcp?x=1", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || string(body) != "ok" {
			t.Fatalf("status %d body %q", response.StatusCode, body)
		}
	}
	waitPaused := func() {
		select {
		case <-s.paused:
		case <-time.After(2 * time.Second):
			t.Fatal("sandbox was not paused after the call")
		}
	}
	post()
	waitPaused()
	// A GET carries the server's notification stream, not a call: it must not wake the sandbox.
	response, err := http.Get(front.URL + "/s/sandbox-a/8931/mcp")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if _, resumes := s.counts(); resumes != 0 {
		t.Fatalf("GET resumed the sandbox")
	}
	post()
	waitPaused()
	if pauses, resumes := s.counts(); pauses != 2 || resumes != 1 {
		t.Fatalf("pauses %d resumes %d", pauses, resumes)
	}
	gate.Close(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 || seen[0] != "POST sandbox-a:8931 /mcp?x=1" || seen[1] != "GET sandbox-a:8931 /mcp" {
		t.Fatalf("upstream saw %q", seen)
	}
}

func TestMCPProxyRefusesUnregisteredSandboxes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an unregistered sandbox must not be reached")
	}))
	defer upstream.Close()
	p, front := testProxy(t, upstream)
	gate, _ := suspend.New(suspend.Options{})
	p.add("sandbox-a", gate)
	p.remove("sandbox-a")
	for _, path := range []string{"/s/sandbox-a/8931/mcp", "/s/sandbox-b/8931/mcp", "/other"} {
		response, err := http.Post(front.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status %d", path, response.StatusCode)
		}
	}
}

func TestMCPProxyStreamsEventsAsTheyArrive(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: second\n\n")
	}))
	defer upstream.Close()
	defer close(release)
	p, front := testProxy(t, upstream)
	gate, _ := suspend.New(suspend.Options{})
	p.add("sandbox-a", gate)
	response, err := http.Post(front.URL+"/s/sandbox-a/8931/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("the first event must arrive before the stream ends: %q %v", line, err)
	}
}

type addressRuntime struct{ lifecycleRuntime }

func (*addressRuntime) TaskAddress(_ context.Context, id string, port int) (string, error) {
	return fmt.Sprintf("10.0.0.2:%d", port), nil
}

func TestSessionPointsMCPServersAtTheProxy(t *testing.T) {
	service := &Service{options: Options{Runtime: &addressRuntime{}, Launch: LaunchSpec{Config: LaunchConfig{MCPProxyPort: 8932, Suspend: core.SandboxSuspend{Mode: core.SuspendPause}}}}, runtimeID: "bridge"}
	s := &Session{service: service, descriptor: core.BridgeTarget{SandboxID: "aries-task-1"}}
	got, err := s.proxiedMCPServers(context.Background(), []core.MCPServerConfig{{Name: "playwright", URL: "http://aries-task-1:8931/mcp", Transport: "streamable-http", TimeoutSeconds: 60}})
	if err != nil {
		t.Fatal(err)
	}
	want := core.MCPServerConfig{Name: "playwright", URL: "http://10.0.0.2:8932/s/aries-task-1/8931/mcp", Transport: "streamable-http", TimeoutSeconds: 60}
	if len(got) != 1 || got[0].URL != want.URL || got[0].Name != want.Name || got[0].Transport != want.Transport || got[0].TimeoutSeconds != want.TimeoutSeconds {
		t.Fatalf("got %+v", got)
	}
	for _, foreign := range []string{"http://other-sandbox:8931/mcp", "https://aries-task-1:8931/mcp", "http://aries-task-1/mcp"} {
		if _, err := s.proxiedMCPServers(context.Background(), []core.MCPServerConfig{{Name: "x", URL: foreign}}); err == nil {
			t.Fatalf("accepted %s", foreign)
		}
	}
}

func TestEvidenceNamesIncludeSuspendRecordOnlyWhenPausing(t *testing.T) {
	if got := strings.Join(EvidenceNames(false, core.SandboxSuspend{Mode: core.SuspendProxy}), ","); got != "tool-calls.jsonl" {
		t.Fatal(got)
	}
	if got := strings.Join(EvidenceNames(true, core.SandboxSuspend{Mode: core.SuspendPause}), ","); got != "tool-calls.jsonl,ssh_raw.log,suspend.jsonl" {
		t.Fatal(got)
	}
}

// Every file a pausing bridge lists must pass evidence finalization, or
// release fails and blocks evaluation.
func TestEvidenceNamesAreFinalizable(t *testing.T) {
	root := t.TempDir()
	names := EvidenceNames(true, core.SandboxSuspend{Mode: core.SuspendPause})
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	artifacts, err := finalizeArtifacts(root, names)
	if err != nil || len(artifacts) != len(names) {
		t.Fatalf("artifacts %+v err %v", artifacts, err)
	}
}
