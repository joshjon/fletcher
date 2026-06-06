package main

import (
	"context"
	"net"
	"net/http"

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

// clientTarget resolves where Connect clients should talk to: a remote daemon
// over the tunnel (`--remote host:port --token ...`, sending a bearer token) or
// the local Unix socket (`--socket`, the default).
func clientTarget(cmd *cli.Command) (connect.HTTPClient, string, []connect.ClientOption) {
	if remote := cmd.String("remote"); remote != "" {
		return &http.Client{}, "http://" + remote,
			[]connect.ClientOption{connect.WithInterceptors(bearerAuthInterceptor(cmd.String("token")))}
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

func newSessionsClient(cmd *cli.Command) fletcherv1connect.SessionServiceClient {
	hc, base, opts := clientTarget(cmd)
	return fletcherv1connect.NewSessionServiceClient(hc, base, opts...)
}
