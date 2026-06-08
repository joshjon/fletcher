package egress

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/joshjon/fletcher/internal/netguard"
)

// Proxy is an HTTP/HTTPS forward-proxy. It serves CONNECT (raw TLS tunnels,
// kept end-to-end so the agent validates the genuine remote certificate) and
// absolute-form plain-HTTP requests, gating each on its Policy and the shared
// netguard LAN guard, and logging every attempt for audit. It satisfies
// http.Handler, so the daemon serves it over the same loopback->vsock relay as
// the gateway and MCP surfaces.
type Proxy struct {
	policy    Policy
	dialer    *net.Dialer
	transport *http.Transport
	logger    *slog.Logger
}

// Option configures a Proxy.
type Option func(*Proxy)

// WithDialer overrides the dialer. Tests inject one without the LAN guard so
// they can reach loopback fixtures; production uses the guarded default.
func WithDialer(d *net.Dialer) Option {
	return func(p *Proxy) { p.dialer = d }
}

// New builds a Proxy enforcing policy. By default it dials through the shared
// netguard guard, so even an Open policy cannot reach private/LAN/metadata
// addresses - a fork can never use the proxy to pivot into the operator's
// network.
func New(policy Policy, logger *slog.Logger, opts ...Option) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Proxy{
		policy: policy,
		dialer: &net.Dialer{Timeout: 15 * time.Second, Control: netguard.DialControl},
		logger: logger,
	}
	for _, o := range opts {
		o(p)
	}
	p.transport = &http.Transport{
		DialContext:           p.dialer.DialContext,
		MaxIdleConns:          32,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return p
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

// handleConnect tunnels a CONNECT request: validate the target host, dial it
// (the LAN guard runs in the dialer), then hijack the client connection and
// splice bytes both ways. TLS rides over the tunnel untouched.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	if !p.policy.Allow(host) {
		p.audit(r.Context(), http.MethodConnect, host, "denied", "policy")
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}
	upstream, err := p.dialer.DialContext(r.Context(), "tcp", r.Host)
	if err != nil {
		p.audit(r.Context(), http.MethodConnect, host, dialDisposition(err), err.Error())
		http.Error(w, "egress dial failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()
	p.audit(r.Context(), http.MethodConnect, host, "allowed", "")

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connect not supported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	splice(client, upstream)
}

// handleHTTP forwards an absolute-form plain-HTTP proxy request.
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "proxy requires an absolute-form request URI", http.StatusBadRequest)
		return
	}
	host := hostOnly(r.URL.Host)
	if !p.policy.Allow(host) {
		p.audit(r.Context(), r.Method, host, "denied", "policy")
		http.Error(w, "egress denied by policy", http.StatusForbidden)
		return
	}

	outReq := r.Clone(r.Context())
	outReq.RequestURI = ""
	outReq.Header.Del("Proxy-Connection")
	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		p.audit(r.Context(), r.Method, host, dialDisposition(err), err.Error())
		http.Error(w, "egress request failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	p.audit(r.Context(), r.Method, host, "allowed", "")

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// audit emits one log line per egress attempt with its final disposition:
// "allowed" (policy passed and the dial succeeded), "denied" (policy rejected
// the host), or "blocked" (the dial-time LAN/metadata guard refused it). This
// is logged after the dial, so a guard-blocked request is never recorded as
// allowed.
func (p *Proxy) audit(ctx context.Context, method, host, disposition, reason string) {
	lvl := slog.LevelInfo
	if disposition != "allowed" {
		lvl = slog.LevelWarn
	}
	attrs := []any{
		slog.String("policy", p.policy.Name()),
		slog.String("method", method),
		slog.String("host", host),
		slog.String("disposition", disposition),
	}
	if reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	p.logger.Log(ctx, lvl, "egress", attrs...)
}

// dialDisposition classifies a failed dial for the audit log: "blocked" when
// the LAN/metadata guard refused it, "error" for any other dial failure.
func dialDisposition(err error) string {
	if errors.Is(err, netguard.ErrBlocked) {
		return "blocked"
	}
	return "error"
}

// hostOnly strips any :port from a host[:port] value.
func hostOnly(hostport string) string {
	if hostport == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// splice copies bidirectionally between two connections and returns once
// either direction ends; the callers' deferred Close on both conns unblocks
// the other copy.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) { _, _ = io.Copy(dst, src); done <- struct{}{} }
	go cp(a, b)
	go cp(b, a)
	<-done
}
