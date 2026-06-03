package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/api"
	"github.com/joshjon/fletcher/internal/errs"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// scriptedAdmin is a stand-in AdminService that the interceptor tests can
// drive: each Health call returns whatever is in next.
type scriptedAdmin struct {
	fletcherv1connect.UnimplementedAdminServiceHandler
	next func(ctx context.Context) (*fletcherv1.HealthResponse, error)
}

func (s *scriptedAdmin) Health(ctx context.Context, _ *connect.Request[fletcherv1.HealthRequest]) (*connect.Response[fletcherv1.HealthResponse], error) {
	resp, err := s.next(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func startTestServer(t *testing.T, admin *scriptedAdmin, logger *slog.Logger) (fletcherv1connect.AdminServiceClient, func()) {
	t.Helper()
	mux := http.NewServeMux()
	path, h := fletcherv1connect.NewAdminServiceHandler(
		admin,
		connect.WithInterceptors(
			api.RequestIDInterceptor(),
			api.ErrorInterceptor(logger),
		),
	)
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	client := fletcherv1connect.NewAdminServiceClient(http.DefaultClient, srv.URL)
	return client, srv.Close
}

func TestErrorInterceptorMapsNotFound(t *testing.T) {
	admin := &scriptedAdmin{
		next: func(ctx context.Context) (*fletcherv1.HealthResponse, error) {
			return nil, errs.New(errs.CategoryNotFound, "no such admin")
		},
	}
	client, stop := startTestServer(t, admin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(stop)

	_, err := client.Health(context.Background(), connect.NewRequest(&fletcherv1.HealthRequest{}))
	require.Error(t, err)
	var ce *connect.Error
	require.True(t, errors.As(err, &ce))
	require.Equal(t, connect.CodeNotFound, ce.Code())
	require.Contains(t, ce.Message(), "no such admin")
}

func TestErrorInterceptorMapsInvalidArgument(t *testing.T) {
	admin := &scriptedAdmin{
		next: func(ctx context.Context) (*fletcherv1.HealthResponse, error) {
			return nil, errs.New(errs.CategoryInvalidArgument, "bad arg")
		},
	}
	client, stop := startTestServer(t, admin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(stop)

	_, err := client.Health(context.Background(), connect.NewRequest(&fletcherv1.HealthRequest{}))
	var ce *connect.Error
	require.True(t, errors.As(err, &ce))
	require.Equal(t, connect.CodeInvalidArgument, ce.Code())
}

func TestErrorInterceptorSanitisesUncategorisedErrors(t *testing.T) {
	admin := &scriptedAdmin{
		next: func(ctx context.Context) (*fletcherv1.HealthResponse, error) {
			return nil, errors.New("secret internal detail")
		},
	}
	client, stop := startTestServer(t, admin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(stop)

	_, err := client.Health(context.Background(), connect.NewRequest(&fletcherv1.HealthRequest{}))
	var ce *connect.Error
	require.True(t, errors.As(err, &ce))
	require.Equal(t, connect.CodeInternal, ce.Code())
	require.NotContains(t, ce.Message(), "secret internal detail")
}

func TestRequestIDInterceptorAttachesIDToContext(t *testing.T) {
	var seen string
	admin := &scriptedAdmin{
		next: func(ctx context.Context) (*fletcherv1.HealthResponse, error) {
			seen = api.RequestID(ctx)
			return &fletcherv1.HealthResponse{Status: "ok"}, nil
		},
	}
	client, stop := startTestServer(t, admin, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(stop)

	resp, err := client.Health(context.Background(), connect.NewRequest(&fletcherv1.HealthRequest{}))
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Msg.GetStatus())
	require.NotEmpty(t, seen, "request id should be set in handler ctx")
}

func TestContextLogHandlerAddsRequestIDAttribute(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewTextHandler(&buf, nil)
	h := api.NewContextLogHandler(base)
	logger := slog.New(h)
	ctx := api.WithRequestID(context.Background(), "req_abc")
	logger.InfoContext(ctx, "hello")
	require.Contains(t, buf.String(), "request_id=req_abc")
}

// Compile-time check: keep net package referenced for future tests that
// dial unix sockets here (avoids unused-import drift if Phase 5 adds them).
var _ = net.Listen
