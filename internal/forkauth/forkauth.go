// Package forkauth authenticates host-stamped fork identity on daemon service forwards.
package forkauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/joshjon/fletcher/internal/background"
)

const identityHeader = "X-Fletcher-Fork-Identity"

// Caller is the identity and policy supplied by the host runtime, never by guest arguments.
type Caller struct {
	ID      string
	Policy  string
	Session bool
}

type callerKey struct{}

// WithCaller attributes trusted in-process calls; network calls must use Authority.Authenticate.
func WithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, caller)
}

// FromContext returns authenticated fork attribution when present.
func FromContext(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerKey{}).(Caller)
	return caller, ok && caller.ID != ""
}

// Authority holds a per-process signing secret that never enters a guest.
type Authority struct{ key [32]byte }

// New creates a fresh signing authority for the daemon lifetime.
func New() *Authority {
	a := &Authority{}
	_, _ = rand.Read(a.key[:]) // crypto/rand.Read fills the buffer or terminates
	return a
}

func (a *Authority) sign(caller Caller) string {
	data, _ := json.Marshal(caller) // only strings and a boolean
	encoded := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *Authority) verify(token string) (Caller, bool) {
	encoded, signature, ok := strings.Cut(token, ".")
	if !ok || len(token) > 4096 {
		return Caller{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return Caller{}, false
	}
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(encoded))
	if !hmac.Equal(got, mac.Sum(nil)) {
		return Caller{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Caller{}, false
	}
	var caller Caller
	if json.Unmarshal(data, &caller) != nil || caller.ID == "" {
		return Caller{}, false
	}
	return caller, true
}

// Authenticate rejects unstamped requests and attributes valid calls to their host-assigned fork.
func (a *Authority) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, ok := a.verify(r.Header.Get(identityHeader))
		if !ok {
			http.Error(w, "authenticated fork identity required", http.StatusUnauthorized)
			return
		}
		r.Header.Del(identityHeader)
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next.ServeHTTP(w, r.WithContext(WithCaller(r.Context(), caller)))
	})
}

// Forward serves a host-side HTTP relay that replaces guest identity headers with trusted runtime attribution.
func (a *Authority) Forward(ctx context.Context, path, upstream string, caller Caller) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	token := a.sign(caller)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", upstream)
	}}
	proxy := &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(r *httputil.ProxyRequest) {
		r.Out.URL.Scheme, r.Out.URL.Host = "http", "localhost"
		r.Out.Header.Set(identityHeader, token)
	}}
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	closed := &forwardListener{Listener: ln, server: srv}
	stop := context.AfterFunc(ctx, func() { _ = closed.Close() })
	background.GoNamed(ctx, "forkauth.forward", func(context.Context) {
		defer stop()
		defer transport.CloseIdleConnections()
		_ = srv.Serve(ln)
	})
	return closed, nil
}

type forwardListener struct {
	net.Listener
	server *http.Server
}

func (l *forwardListener) Close() error {
	err := l.server.Close()
	if closeErr := l.Listener.Close(); !errors.Is(closeErr, net.ErrClosed) {
		err = errors.Join(err, closeErr)
	}
	return err
}
