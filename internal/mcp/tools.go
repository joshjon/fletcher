package mcp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/joshjon/fletcher/internal/buildinfo"
)

// daemonHealthTool reports the daemon's build identity and uptime. Trivial,
// no real privileged action — exists primarily to give MCP clients a
// "is the gateway alive?" probe that follows the same audit seam as the
// privileged tools.
func daemonHealthTool(startedAt time.Time) Tool {
	return Tool{
		Spec: mcpgo.NewTool("daemon_health",
			mcpgo.WithDescription("Return the daemon's build info and uptime."),
		),
		Handler: func(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			info := buildinfo.Info()
			uptime := time.Since(startedAt).Round(time.Second)
			return mcpgo.NewToolResultText(fmt.Sprintf(
				"version=%s commit=%s built=%s uptime=%s",
				info.Version, info.Commit, info.Date, uptime,
			)), nil
		},
	}
}

// httpGetTool is the daemon-mediated egress shim. Agents inside a job's
// fork have no network egress (per DESIGN.md §5); this tool lets them ask
// the daemon to perform a GET on their behalf. Real egress policy will be
// layered on as approvals + allowlists land in later phases — for now any
// http/https URL is allowed.
func httpGetTool(httpClient *http.Client) Tool {
	return Tool{
		Spec: mcpgo.NewTool("http_get",
			mcpgo.WithDescription("Perform an HTTP GET request through the daemon. Returns the response body as text."),
			mcpgo.WithString("url",
				mcpgo.Required(),
				mcpgo.Description("Absolute http or https URL to fetch."),
			),
		),
		Handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			url, err := req.RequireString("url")
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if err := validateEgressURL(url); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}

			httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("build request: %s", err)), nil
			}
			resp, err := httpClient.Do(httpReq)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("perform request: %s", err)), nil
			}
			defer func() { _ = resp.Body.Close() }()

			// Cap body size to avoid runaway memory; agents fetching anything
			// huge should be using a streaming tool we'll add later.
			const maxBody = 256 * 1024
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("read body: %s", err)), nil
			}
			return mcpgo.NewToolResultText(fmt.Sprintf("status=%d\n%s", resp.StatusCode, string(body))), nil
		},
	}
}

// validateEgressURL rejects URLs we never want the gateway to fetch. For
// phase 6 the bar is low — scheme must be http(s); future phases will
// likely refuse loopback / link-local / metadata endpoints to stop SSRF
// against the daemon's own surface.
func validateEgressURL(_ string) error {
	// Intentionally permissive for phase 6. Hardening lands when egress
	// approvals do.
	return nil
}

// RegisterBuiltinTools wires Fletcher's standard tool set onto srv. Future
// phases (approvals, secrets-bound tools, ...) extend this list.
func RegisterBuiltinTools(srv *Server, startedAt time.Time, httpClient *http.Client) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	srv.RegisterTool(daemonHealthTool(startedAt))
	srv.RegisterTool(httpGetTool(httpClient))
}
