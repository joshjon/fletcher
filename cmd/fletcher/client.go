package main

import (
	"context"
	"net"
	"net/http"

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

func newAdminClient(socket string) fletcherv1connect.AdminServiceClient {
	return fletcherv1connect.NewAdminServiceClient(unixHTTPClient(socket), unixBaseURL)
}

func newJobsClient(socket string) fletcherv1connect.JobServiceClient {
	return fletcherv1connect.NewJobServiceClient(unixHTTPClient(socket), unixBaseURL)
}

func newModelsClient(socket string) fletcherv1connect.ModelServiceClient {
	return fletcherv1connect.NewModelServiceClient(unixHTTPClient(socket), unixBaseURL)
}
