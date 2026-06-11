package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/joshjon/fletcher/internal/approval"
	"github.com/joshjon/fletcher/internal/buildinfo"
	"github.com/joshjon/fletcher/internal/netguard"
)

// maxEgressBody caps how much of an egress response body the daemon reads back
// to a fork, so a single fetch cannot exhaust daemon memory. Shared by the
// GET shim and the general request tool.
const maxEgressBody = 256 * 1024

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

			body, err := io.ReadAll(io.LimitReader(resp.Body, maxEgressBody))
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("read body: %s", err)), nil
			}
			return mcpgo.NewToolResultText(fmt.Sprintf("status=%d\n%s", resp.StatusCode, string(body))), nil
		},
	}
}

// httpRequestTool generalises httpGetTool to non-GET methods so agents can call
// JSON APIs (search backends, REST endpoints) through the daemon, still behind
// the SSRF guard in NewEgressHTTPClient. Method defaults to GET; body and
// headers are optional; the response body is size-capped like http_get.
// Per-host egress policy (the Phase B forward-proxy) layers on top of this.
func httpRequestTool(httpClient *http.Client) Tool {
	return Tool{
		Spec: mcpgo.NewTool("http_request",
			mcpgo.WithDescription("Perform an HTTP request through the daemon (the fork has no direct network egress). Supports GET/POST/PUT/PATCH/DELETE/HEAD with an optional body and headers. Returns the status and response body as text."),
			mcpgo.WithString("url",
				mcpgo.Required(),
				mcpgo.Description("Absolute http or https URL."),
			),
			mcpgo.WithString("method",
				mcpgo.Description("HTTP method: GET, POST, PUT, PATCH, DELETE, or HEAD. Defaults to GET."),
			),
			mcpgo.WithString("body",
				mcpgo.Description("Optional request body sent as-is."),
			),
			mcpgo.WithString("headers_json",
				mcpgo.Description(`Optional request headers as a JSON object, e.g. {"content-type":"application/json"}.`),
			),
		),
		Handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			rawURL, err := req.RequireString("url")
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if err := validateEgressURL(rawURL); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			method := strings.ToUpper(req.GetString("method", http.MethodGet))
			if !allowedEgressMethod(method) {
				return mcpgo.NewToolResultError(fmt.Sprintf("unsupported method %q", method)), nil
			}

			var bodyReader io.Reader = http.NoBody
			if b := req.GetString("body", ""); b != "" {
				bodyReader = strings.NewReader(b)
			}
			httpReq, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("build request: %s", err)), nil
			}
			if hj := req.GetString("headers_json", ""); hj != "" {
				var hdrs map[string]string
				if err := json.Unmarshal([]byte(hj), &hdrs); err != nil {
					return mcpgo.NewToolResultError(fmt.Sprintf("headers_json must be a JSON object of string values: %s", err)), nil
				}
				for k, v := range hdrs {
					httpReq.Header.Set(k, v)
				}
			}

			resp, err := httpClient.Do(httpReq)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("perform request: %s", err)), nil
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(io.LimitReader(resp.Body, maxEgressBody))
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("read body: %s", err)), nil
			}
			return mcpgo.NewToolResultText(fmt.Sprintf("status=%d\n%s", resp.StatusCode, string(body))), nil
		},
	}
}

// allowedEgressMethod restricts the general request tool to ordinary HTTP
// methods, so an agent cannot smuggle an arbitrary verb through the daemon.
func allowedEgressMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
		return true
	default:
		return false
	}
}

// validateEgressURL is the cheap, no-network policy check on an egress URL:
// it must be an absolute http(s) URL with a host. The authoritative SSRF guard
// (refusing loopback / link-local / private / cloud-metadata targets) runs at
// dial time in NewEgressHTTPClient, so it cannot be bypassed by DNS rebinding
// or a hostname that resolves to an internal address.
func validateEgressURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("egress URL must be http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("egress URL must have a host")
	}
	return nil
}

// NewEgressHTTPClient builds the HTTP client the daemon-mediated egress tool
// uses. Its dialer refuses to connect to any non-global address (the shared
// netguard SSRF guard), checked against the IP actually being dialed (after
// DNS), so an agent in a fork cannot use the daemon to reach its own loopback
// admin surface, the cloud-metadata endpoint, or the operator's LAN.
func NewEgressHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: netguard.DialControl,
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}

// ImagePublisher is the subset of the daemon's session/image machinery the
// publish_image tool needs: committing a session's fork as a template, and the
// server-side registry import. Implemented by the daemon.
type ImagePublisher interface {
	// CommitSessionImage commits the referenced session's fork as an image
	// template and returns the template name.
	CommitSessionImage(ctx context.Context, p CommitImage) (string, error)
	// ImportRegistryImage pulls ref and flattens it into an image template,
	// returning the template name.
	ImportRegistryImage(ctx context.Context, ref, name string, force bool) (string, error)
}

// CommitImage parameterises an ImagePublisher session commit.
type CommitImage struct {
	SessionRef  string
	Name        string
	Entrypoint  []string
	Cmd         []string
	WorkingDir  string
	ExposedPort int
	Force       bool
}

// publishWaitDefault and publishWaitMax bound how long publish_image blocks on
// the operator's decision.
const (
	publishWaitDefault = 10 * time.Minute
	publishWaitMax     = time.Hour
)

// publishImageTool lets an agent publish an image template to the daemon - by
// committing its own session's disk (the local-first path: nothing leaves the
// network) or by importing a registry ref. Always approval-gated: the call
// creates a pending approval and blocks until the operator decides, so nothing
// is published silently. The session identity is agent-claimed (the daemon
// injects FLETCHER_SESSION_NAME into each session); the approval card carries
// the resolved ground truth the operator decides on.
func publishImageTool(publisher ImagePublisher, approvals ApprovalBackend) Tool {
	return Tool{
		Spec: mcpgo.NewTool("publish_image",
			mcpgo.WithDescription("Publish an image template to the Fletcher daemon so the operator can deploy it or create sessions from it. Requires human approval: the call blocks until the operator approves or denies. Either commit the calling session's disk as the image (set session to the value of $FLETCHER_SESSION_NAME) or import an image from a registry (set source_ref)."),
			mcpgo.WithString("name",
				mcpgo.Required(),
				mcpgo.Description("Template name for the published image (lowercase letters, digits, '.', '_', '-')."),
			),
			mcpgo.WithString("justification",
				mcpgo.Required(),
				mcpgo.Description("Why the image should be published - shown to the operator on the approval."),
			),
			mcpgo.WithString("session",
				mcpgo.Description("Session to commit as the image. Pass the value of $FLETCHER_SESSION_NAME. Mutually exclusive with source_ref."),
			),
			mcpgo.WithString("source_ref",
				mcpgo.Description("Registry image ref to import instead (e.g. ghcr.io/you/app:v1). Mutually exclusive with session."),
			),
			mcpgo.WithString("entrypoint_json",
				mcpgo.Description(`Command a deploy of the image runs, as a JSON array (e.g. ["node","/workspace/server.js"]). Session commits only.`),
			),
			mcpgo.WithString("cmd_json",
				mcpgo.Description("Arguments appended to entrypoint, as a JSON array. Session commits only."),
			),
			mcpgo.WithString("working_dir",
				mcpgo.Description("Working directory the deploy's app starts in. Session commits only."),
			),
			mcpgo.WithNumber("exposed_port",
				mcpgo.Description("Port a deploy of the image publishes by default."),
			),
			mcpgo.WithBoolean("force",
				mcpgo.Description("Replace an existing template of the same name."),
			),
			mcpgo.WithNumber("wait_seconds",
				mcpgo.Description("How long to block waiting for the operator's decision. Defaults to 600, capped at 3600."),
			),
		),
		Handler: func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			args, problem := parsePublishArgs(req)
			if problem != "" {
				return mcpgo.NewToolResultError(problem), nil
			}

			action := fmt.Sprintf("publish image %q from registry ref %q", args.name, args.sourceRef)
			requester := "agent"
			if args.session != "" {
				action = fmt.Sprintf("publish image %q by committing session %q", args.name, args.session)
				requester = "agent:" + args.session
			}
			created, err := approvals.Create(ctx, approval.CreateParams{
				Action:        action,
				Justification: args.justification,
				Requester:     requester,
				TTL:           args.wait,
			})
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			final, err := approvals.Wait(ctx, created.ID)
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("wait for approval %s: %s", created.ID, err)), nil
			}
			if final.Status != approval.StatusApproved {
				return mcpgo.NewToolResultText(fmt.Sprintf(
					"image not published: approval %s is %s%s", final.ID, final.Status, reasonSuffix(final))), nil
			}

			var published string
			if args.session != "" {
				published, err = publisher.CommitSessionImage(ctx, CommitImage{
					SessionRef:  args.session,
					Name:        args.name,
					Entrypoint:  args.entrypoint,
					Cmd:         args.cmd,
					WorkingDir:  args.workingDir,
					ExposedPort: args.exposedPort,
					Force:       args.force,
				})
			} else {
				published, err = publisher.ImportRegistryImage(ctx, args.sourceRef, args.name, args.force)
			}
			if err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("approval %s approved, but publishing failed: %s", final.ID, err)), nil
			}
			return mcpgo.NewToolResultText(fmt.Sprintf(
				"published image %q (approval %s). The operator can now deploy it or create sessions from it.",
				published, final.ID)), nil
		},
	}
}

// publishArgs are the parsed and validated arguments of a publish_image call.
type publishArgs struct {
	name          string
	justification string
	session       string
	sourceRef     string
	entrypoint    []string
	cmd           []string
	workingDir    string
	exposedPort   int
	force         bool
	wait          time.Duration
}

// parsePublishArgs validates a publish_image request. A non-empty problem is
// the user-facing reason the arguments do not make sense together.
func parsePublishArgs(req mcpgo.CallToolRequest) (publishArgs, string) {
	var args publishArgs
	var err error
	if args.name, err = req.RequireString("name"); err != nil {
		return args, err.Error()
	}
	if args.justification, err = req.RequireString("justification"); err != nil {
		return args, err.Error()
	}
	args.session = strings.TrimSpace(req.GetString("session", ""))
	args.sourceRef = strings.TrimSpace(req.GetString("source_ref", ""))
	if (args.session == "") == (args.sourceRef == "") {
		return args, "set exactly one of session (commit this session's disk) or source_ref (import from a registry)"
	}
	if args.entrypoint, err = parseJSONArray(req.GetString("entrypoint_json", "")); err != nil {
		return args, fmt.Sprintf("entrypoint_json: %s", err)
	}
	if args.cmd, err = parseJSONArray(req.GetString("cmd_json", "")); err != nil {
		return args, fmt.Sprintf("cmd_json: %s", err)
	}
	args.workingDir = req.GetString("working_dir", "")
	if args.sourceRef != "" && (len(args.entrypoint) > 0 || len(args.cmd) > 0 || args.workingDir != "") {
		return args, "entrypoint_json/cmd_json/working_dir apply to session commits only (a registry image carries its own run config)"
	}
	args.exposedPort = int(req.GetFloat("exposed_port", 0))
	args.force = req.GetBool("force", false)
	args.wait = time.Duration(req.GetFloat("wait_seconds", publishWaitDefault.Seconds())) * time.Second
	if args.wait <= 0 {
		args.wait = publishWaitDefault
	}
	if args.wait > publishWaitMax {
		args.wait = publishWaitMax
	}
	return args, ""
}

// parseJSONArray decodes an optional JSON array of strings ("" -> nil).
func parseJSONArray(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("must be a JSON array of strings: %w", err)
	}
	return out, nil
}

// reasonSuffix formats an approval's decision reason for a tool result.
func reasonSuffix(a approval.Approval) string {
	if a.DecisionReason == "" {
		return ""
	}
	return " (reason: " + a.DecisionReason + ")"
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
// publisher may be nil (no session-capable runtime), which skips publish_image.
func RegisterBuiltinTools(srv *Server, startedAt time.Time, httpClient *http.Client, approvals ApprovalBackend, publisher ImagePublisher) {
	if httpClient == nil {
		httpClient = NewEgressHTTPClient(30 * time.Second)
	}
	srv.RegisterTool(daemonHealthTool(startedAt))
	srv.RegisterTool(httpGetTool(httpClient))
	srv.RegisterTool(httpRequestTool(httpClient))
	if approvals != nil {
		srv.RegisterTool(requestApprovalTool(approvals))
		if publisher != nil {
			srv.RegisterTool(publishImageTool(publisher, approvals))
		}
	}
}
