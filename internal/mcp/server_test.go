package mcp_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/audit"
	fletchermcp "github.com/joshjon/fletcher/internal/mcp"
)

// recordingRecorder captures every audit event for inspection.
type recordingRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordingRecorder) Record(_ context.Context, e audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recordingRecorder) snapshot() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.Event, len(r.events))
	copy(out, r.events)
	return out
}

func newTestServerWithBuiltins(t *testing.T, upstream *httptest.Server, recorder audit.Recorder) *fletchermcp.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := fletchermcp.NewServer("fletcher-test", "0.0.0", recorder, logger)
	httpClient := upstream.Client()
	fletchermcp.RegisterBuiltinTools(srv, time.Now(), httpClient, nil)
	return srv
}

func TestDaemonHealthToolReturnsBuildInfo(t *testing.T) {
	r := &recordingRecorder{}
	srv := newTestServerWithBuiltins(t, httptest.NewServer(nil), r)

	c, err := client.NewInProcessClient(srv.Inner())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcpgo.InitializeRequest{})
	require.NoError(t, err)

	resp, err := c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "daemon_health"},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "tool returned error: %v", resp.Content)
	require.Contains(t, textOf(resp), "version=")
	require.Contains(t, textOf(resp), "uptime=")

	// Audit event was recorded.
	events := r.snapshot()
	require.NotEmpty(t, events)
	require.Equal(t, "mcp.tool_call", events[0].Kind)
	require.Equal(t, "daemon_health", events[0].Subject)
}

func TestHTTPGetToolPerformsEgressAndRecordsAudit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello from upstream")
	}))
	t.Cleanup(upstream.Close)

	r := &recordingRecorder{}
	srv := newTestServerWithBuiltins(t, upstream, r)

	c, err := client.NewInProcessClient(srv.Inner())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcpgo.InitializeRequest{})
	require.NoError(t, err)

	resp, err := c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{
			Name:      "http_get",
			Arguments: map[string]any{"url": upstream.URL},
		},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "tool returned error: %v", resp.Content)
	require.Contains(t, textOf(resp), "status=200")
	require.Contains(t, textOf(resp), "hello from upstream")

	events := r.snapshot()
	require.NotEmpty(t, events)
	require.Equal(t, "http_get", events[len(events)-1].Subject)
	require.Equal(t, upstream.URL, events[len(events)-1].Detail["arguments"].(map[string]any)["url"])
}

func TestHTTPGetToolRequiresURL(t *testing.T) {
	srv := newTestServerWithBuiltins(t, httptest.NewServer(nil), audit.Noop{})

	c, err := client.NewInProcessClient(srv.Inner())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcpgo.InitializeRequest{})
	require.NoError(t, err)

	resp, err := c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Name: "http_get"},
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
}

func TestHTTPRequestToolSendsMethodBodyAndHeaders(t *testing.T) {
	var (
		gotMethod      string
		gotBody        string
		gotContentType string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotMethod = req.Method
		gotContentType = req.Header.Get("Content-Type")
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, "created")
	}))
	t.Cleanup(upstream.Close)

	r := &recordingRecorder{}
	srv := newTestServerWithBuiltins(t, upstream, r)

	c, err := client.NewInProcessClient(srv.Inner())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcpgo.InitializeRequest{})
	require.NoError(t, err)

	resp, err := c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{
			Name: "http_request",
			Arguments: map[string]any{
				"url":          upstream.URL,
				"method":       "post",
				"body":         `{"q":"hi"}`,
				"headers_json": `{"content-type":"application/json"}`,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "tool returned error: %v", resp.Content)
	require.Contains(t, textOf(resp), "status=200")
	require.Contains(t, textOf(resp), "created")
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, `{"q":"hi"}`, gotBody)
	require.Equal(t, "application/json", gotContentType)

	events := r.snapshot()
	require.Equal(t, "http_request", events[len(events)-1].Subject)
}

func TestHTTPRequestToolRejectsBadMethod(t *testing.T) {
	srv := newTestServerWithBuiltins(t, httptest.NewServer(nil), audit.Noop{})

	c, err := client.NewInProcessClient(srv.Inner())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcpgo.InitializeRequest{})
	require.NoError(t, err)

	resp, err := c.CallTool(ctx, mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{
			Name:      "http_request",
			Arguments: map[string]any{"url": "https://example.com", "method": "CONNECT"},
		},
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
}

func textOf(r *mcpgo.CallToolResult) string {
	var out string
	for _, c := range r.Content {
		if t, ok := c.(mcpgo.TextContent); ok {
			out += t.Text
		}
	}
	return out
}
