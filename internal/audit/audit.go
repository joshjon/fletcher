// Package audit defines the seam for the daemon's audit log. The seam
// exists from day one so that privileged operations (MCP tool calls,
// approval transitions, egress) can be wrapped uniformly even before
// storage is wired up.
//
// Production code holds a Recorder; the daemon currently configures the
// Noop recorder. When the audit log lands as a SQLite-backed table, a
// real Recorder replaces it - no call sites change.
package audit

import "context"

// Event describes a single privileged operation worth auditing.
type Event struct {
	// Kind is a stable identifier, e.g. "mcp.tool_call", "approval.granted".
	Kind string
	// Subject is the thing the operation applies to (job_id, approval_id, ...).
	Subject string
	// Actor is who initiated the operation (agent ID, user ID, ...).
	Actor string
	// Detail carries small, ad-hoc structured metadata. Implementations
	// must treat values as opaque and avoid logging secrets - callers are
	// expected to redact before passing in.
	Detail map[string]any
}

// Recorder appends Events to the audit log. Implementations must be safe
// for concurrent use. Recording must not fail the calling operation - log
// the error internally and return nil where appropriate.
type Recorder interface {
	Record(ctx context.Context, e Event) error
}

// Noop is the zero-state recorder used until the real audit log lands.
// It discards every event.
type Noop struct{}

// Record discards e and returns nil.
func (Noop) Record(_ context.Context, _ Event) error { return nil }
