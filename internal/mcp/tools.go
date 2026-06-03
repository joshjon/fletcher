package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/joshjon/fletcher/internal/approval"
	"github.com/joshjon/fletcher/internal/buildinfo"
)

// ApprovalBackend is the subset of approval.Service the MCP tool needs.
// Keeping it as an interface lets tests stub it cleanly.
type ApprovalBackend interface {
	Create(ctx context.Context, p approval.CreateParams) (approval.Approval, error)
	Get(ctx context.Context, id string) (approval.Approval, error)
	Wait(ctx context.Context, id string) (approval.Approval, error)
}

// daemonHealthTool reports the daemon's build identity and uptime. Trivial,
// no real privileged action - exists primarily to give MCP clients a
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
// layered on as approvals + allowlists land in later phases - for now any
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
// phase 6 the bar is low - scheme must be http(s); future phases will
// likely refuse loopback / link-local / metadata endpoints to stop SSRF
// against the daemon's own surface.
func validateEgressURL(_ string) error {
	// Intentionally permissive for phase 6. Hardening lands when egress
	// approvals do.
	return nil
}

// requestApprovalTool lets agents ask the human for permission to perform
// a privileged action. When wait_seconds > 0 the call blocks server-side
// until the approval is decided (or its TTL expires); otherwise it
// returns immediately with status=pending and the agent polls via
// approval RPCs.
func requestApprovalTool(approvals ApprovalBackend) Tool {
	return Tool{
		Spec: mcpgo.NewTool("request_approval",
			mcpgo.WithDescription("Ask for human approval of a privileged operation. Returns once the approval is decided or the wait timeout elapses."),
			mcpgo.WithString("action",
				mcpgo.Required(),
				mcpgo.Description("Short description of the operation being requested."),
			),
			mcpgo.WithString("justification",
				mcpgo.Required(),
				mcpgo.Description("Why the action is being requested."),
			),
			mcpgo.WithString("requester",
				mcpgo.Description("Optional identifier of who is asking. Defaults to 'mcp'."),
			),
			mcpgo.WithNumber("wait_seconds",
				mcpgo.Description("How long to block waiting for a decision. 0 returns immediately."),
			),
		),
		Handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			action, err := req.RequireString("action")
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			just, err := req.RequireString("justification")
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			requester := req.GetString("requester", "mcp")
			waitSeconds := req.GetFloat("wait_seconds", 0)

			created, err := approvals.Create(ctx, approval.CreateParams{
				Action:        action,
				Justification: just,
				Requester:     requester,
			})
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}

			if waitSeconds <= 0 {
				return mcpgo.NewToolResultText(formatApproval(created)), nil
			}

			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds*float64(time.Second)))
			defer cancel()
			final, err := approvals.Wait(waitCtx, created.ID)
			if err != nil {
				// Caller-side timeout shouldn't error the tool - return current state.
				if errors.Is(err, context.DeadlineExceeded) {
					current, gerr := approvals.Get(ctx, created.ID)
					if gerr != nil {
						return mcpgo.NewToolResultError(gerr.Error()), nil
					}
					return mcpgo.NewToolResultText(formatApproval(current)), nil
				}
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			return mcpgo.NewToolResultText(formatApproval(final)), nil
		},
	}
}

func formatApproval(a approval.Approval) string {
	out := fmt.Sprintf("id=%s status=%s action=%s", a.ID, a.Status, a.Action)
	if a.DecisionReason != "" {
		out += " reason=" + a.DecisionReason
	}
	return out
}

// RegisterBuiltinTools wires Fletcher's standard tool set onto srv. Future
// phases extend this list (egress allowlists, secrets-bound tools, ...).
func RegisterBuiltinTools(srv *Server, startedAt time.Time, httpClient *http.Client, approvals ApprovalBackend) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	srv.RegisterTool(daemonHealthTool(startedAt))
	srv.RegisterTool(httpGetTool(httpClient))
	if approvals != nil {
		srv.RegisterTool(requestApprovalTool(approvals))
	}
}
