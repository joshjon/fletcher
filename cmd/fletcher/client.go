package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// unixHTTPClient returns an *http.Client that dials the daemon over a Unix
// socket. The base URL passed to Connect clients is meaningless on Unix
// transport - http://unix is conventional.
func unixHTTPClient(socket string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

const unixBaseURL = "http://unix"

// resolveRemote applies the target precedence for driving the daemon: explicit
// --remote/--token flags and the FLETCHER_REMOTE/FLETCHER_TOKEN env vars
// (resolved by urfave/cli) win; otherwise the stored `fletcher login` config is
// used. An empty remote means "use the local Unix socket".
//
// The token falls back to the stored login whenever it is not given explicitly
// and the target matches the logged-in remote - so a stray FLETCHER_REMOTE that
// points at your own daemon still uses your login token instead of silently
// sending none (which the daemon answers with an opaque 401).
func resolveRemote(cmd *cli.Command) (remote, token string) {
	remote, token = cmd.String("remote"), cmd.String("token")
	cfg := loadClientConfig()
	if remote == "" {
		remote = cfg.Remote
	}
	if token == "" && remote != "" && remote == cfg.Remote {
		token = cfg.Token
	}
	return remote, token
}

// warnIfRemoteUnauthed hints when a command will hit a remote daemon with no
// token - almost always a stray FLETCHER_REMOTE or a missing login, which the
// daemon otherwise answers with a bare 401.
func warnIfRemoteUnauthed(remote, token string) {
	if remote != "" && token == "" {
		fmt.Fprintln(os.Stderr, yellow("warning:")+" targeting remote "+remote+
			" with no API token - run `fletcher login`, or unset a stray FLETCHER_REMOTE")
	}
}

// clientTarget resolves where Connect clients should talk to: a remote daemon
// over the tunnel (sending a bearer token) or the local Unix socket (`--socket`,
// the default). The remote target comes from flags, env, or `fletcher login`.
func clientTarget(cmd *cli.Command) (connect.HTTPClient, string, []connect.ClientOption) {
	if remote, token := resolveRemote(cmd); remote != "" {
		warnIfRemoteUnauthed(remote, token)
		return &http.Client{}, "http://" + remote,
			[]connect.ClientOption{connect.WithInterceptors(bearerAuthInterceptor(token))}
	}
	return unixHTTPClient(cmd.String("socket")), unixBaseURL, nil
}

// bearerAuthInterceptor adds `Authorization: Bearer <token>` to outgoing unary
// requests when a token is set, for the network-exposed remote API.
func bearerAuthInterceptor(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			return next(ctx, req)
		}
	}
}

func newAdminClient(cmd *cli.Command) fletcherv1connect.AdminServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewAdminServiceClient(hc, base, opts...)
}

func newJobsClient(cmd *cli.Command) fletcherv1connect.JobServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewJobServiceClient(hc, base, opts...)
}

func newModelsClient(cmd *cli.Command) fletcherv1connect.ModelServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewModelServiceClient(hc, base, opts...)
}

// newSessionsClient builds the SessionService client over cleartext HTTP/2
// (h2c). The interactive shell is a bidi stream, which needs HTTP/2; the
// daemon serves the session API over h2c, so prior-knowledge HTTP/2 here lets
// both the unary verbs and the shell share one transport.
func newSessionsClient(cmd *cli.Command) fletcherv1connect.SessionServiceClient {
	if remote, token := resolveRemote(cmd); remote != "" {
		warnIfRemoteUnauthed(remote, token)
		hc := h2cClient("tcp", remote)
		return fletcherv1connect.NewSessionServiceClient(hc, "http://"+remote,
			connect.WithInterceptors(bearerInterceptor{token: token}))
	}
	hc := h2cClient("unix", cmd.String("socket"))
	return fletcherv1connect.NewSessionServiceClient(hc, unixBaseURL)
}

// h2cClient dials addr on the given network (unix or tcp) speaking
// prior-knowledge HTTP/2 over cleartext - what the daemon's session API serves.
func h2cClient(network, addr string) *http.Client {
	t := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	t.Protocols = new(http.Protocols)
	t.Protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: t}
}

// bearerInterceptor sets Authorization: Bearer on both unary calls and
// streams, for the token-gated remote API. (The unary-only
// bearerAuthInterceptor cannot reach streaming RPCs like the shell.)
type bearerInterceptor struct{ token string }

func (b bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b.token != "" {
			req.Header().Set("Authorization", "Bearer "+b.token)
		}
		return next(ctx, req)
	}
}

func (b bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if b.token != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		}
		return conn
	}
}

func (b bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
