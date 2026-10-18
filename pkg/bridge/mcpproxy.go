package bridge

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/suspend"
)

// MCPProxyPath is the prefix under which the bridge serves a sandbox's MCP
// servers: /s/<sandbox_id>/<port>/<path> forwards to
// http://<sandbox_id>:<port>/<path> on the run network.
const MCPProxyPath = "/s/"

// identifierPattern matches the sandbox IDs bridge control accepts.
var identifierPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,255}$`)

// mcpProxy forwards MCP requests to registered sandboxes only, holding each
// sandbox's gate open for the duration of every request except the
// long-lived GET stream, which carries server notifications rather than calls.
type mcpProxy struct {
	mu        sync.Mutex
	gates     map[string]*suspend.Gate
	server    *http.Server
	transport http.RoundTripper
}

func startMCPProxy(port int) (*mcpProxy, error) {
	if port <= 0 || port > 65535 {
		return nil, errors.New("bridge MCP proxy requires a valid port")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Sandbox traffic stays on the run network.
	p := &mcpProxy{gates: make(map[string]*suspend.Gate), transport: transport}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go p.server.Serve(listener)
	return p, nil
}

func (p *mcpProxy) add(id string, gate *suspend.Gate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gates[id] = gate
}

func (p *mcpProxy) remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.gates, id)
}

func (p *mcpProxy) lookup(id string) *suspend.Gate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gates[id]
}

func (p *mcpProxy) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if p.server.Shutdown(ctx) != nil {
		p.server.Close()
	}
}

// parseMCPProxyPath splits /s/<id>/<port>/<path> into its target.
func parseMCPProxyPath(path string) (id string, port int, rest string, ok bool) {
	trimmed, found := strings.CutPrefix(path, MCPProxyPath)
	if !found {
		return "", 0, "", false
	}
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) < 2 || !identifierPattern.MatchString(parts[0]) {
		return "", 0, "", false
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil || port < 1 || port > 65535 {
		return "", 0, "", false
	}
	rest = "/"
	if len(parts) == 3 {
		rest += parts[2]
	}
	return parts[0], port, rest, true
}

func (p *mcpProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, port, rest, ok := parseMCPProxyPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	gate := p.lookup(id)
	if gate == nil {
		http.Error(w, "sandbox is not registered with the bridge", http.StatusNotFound)
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(id, strconv.Itoa(port))}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.URL.Path, request.Out.URL.RawPath = rest, ""
			request.Out.Host = target.Host
		},
		Transport: p.transport,
		// Streamable HTTP answers a call with an event stream; pass each event on.
		FlushInterval: -1,
	}
	if r.Method != http.MethodGet {
		gate.Acquire(r.Context())
		defer gate.Release()
	}
	proxy.ServeHTTP(w, r)
}
