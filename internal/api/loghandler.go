package api

import (
	"context"
	"log/slog"
)

// ContextLogHandler wraps a slog.Handler so that Records inherit
// request-scoped attributes (currently: request_id) from ctx automatically.
// Code that uses slog.*Context calls picks up correlation with zero
// per-call ceremony.
type ContextLogHandler struct {
	base slog.Handler
}

// NewContextLogHandler wraps base.
func NewContextLogHandler(base slog.Handler) *ContextLogHandler {
	return &ContextLogHandler{base: base}
}

// Enabled mirrors the wrapped handler.
func (h *ContextLogHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.base.Enabled(ctx, lvl)
}

// Handle inspects ctx for known correlation IDs and attaches them as
// attributes before delegating to the wrapped handler.
func (h *ContextLogHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.base.Handle(ctx, r)
}

// WithAttrs returns a handler whose Records always include attrs.
func (h *ContextLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextLogHandler{base: h.base.WithAttrs(attrs)}
}

// WithGroup returns a handler whose Records nest under name.
func (h *ContextLogHandler) WithGroup(name string) slog.Handler {
	return &ContextLogHandler{base: h.base.WithGroup(name)}
}
