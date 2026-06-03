// Package mcp wraps mark3labs/mcp-go's MCP server with Fletcher's audit
// seam: every tool registered here is invoked through a thin shim that
// records an audit event before delegating to the user-supplied handler.
// Tools registered directly on the inner *server.MCPServer would bypass
// the seam — always go through Server.RegisterTool.
package mcp

import (
	"context"
	"log/slog"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/joshjon/fletcher/internal/audit"
)

// Server is Fletcher's MCP façade. Tools register through it so privileged
// invocations route through the audit recorder.
type Server struct {
	inner    *mcpserver.MCPServer
	recorder audit.Recorder
	logger   *slog.Logger
}

// Tool bundles an MCP tool spec with its handler. Use NewTool to build it.
type Tool struct {
	Spec    mcpgo.Tool
	Handler mcpserver.ToolHandlerFunc
}

// NewServer builds an MCP server identified to clients by name + version.
// recorder is the audit sink; pass audit.Noop{} until the SQLite log lands.
func NewServer(name, version string, recorder audit.Recorder, logger *slog.Logger) *Server {
	if recorder == nil {
		recorder = audit.Noop{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	inner := mcpserver.NewMCPServer(
		name, version,
		mcpserver.WithToolCapabilities(true),
	)
	return &Server{inner: inner, recorder: recorder, logger: logger}
}

// Inner returns the underlying *mcpserver.MCPServer. Useful only for
// constructing transports (StreamableHTTPServer, InProcessClient) — do
// not call AddTool on it directly; use Server.RegisterTool so audit fires.
func (s *Server) Inner() *mcpserver.MCPServer { return s.inner }

// RegisterTool installs tool with an audit-wrapped handler.
func (s *Server) RegisterTool(tool Tool) {
	s.inner.AddTool(tool.Spec, s.auditWrap(tool))
}

// auditWrap records an audit event for every tool invocation. The audit
// payload includes the tool name and the caller-supplied arguments; we
// trust upstream redaction (typed wrappers per STANDARDS) to keep secrets
// out of the args.
func (s *Server) auditWrap(tool Tool) mcpserver.ToolHandlerFunc {
	name := tool.Spec.Name
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ev := audit.Event{
			Kind:    "mcp.tool_call",
			Subject: name,
			Detail:  map[string]any{"arguments": req.GetArguments()},
		}
		if err := s.recorder.Record(ctx, ev); err != nil {
			s.logger.WarnContext(ctx, "audit record failed",
				slog.String("tool", name),
				slog.String("err", err.Error()),
			)
		}
		return tool.Handler(ctx, req)
	}
}
