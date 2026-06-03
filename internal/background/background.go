// Package background spawns panic-safe goroutines. The "background.Go"
// helper is the daemon's enforced convention for non-bare goroutines
// (STANDARDS.md): every spawn picks up a structured panic recovery and a
// caller-derived name in logs, with zero call-site ceremony.
package background

import (
	"context"
	"log/slog"
	"runtime/debug"

	"github.com/joshjon/fletcher/internal/fname"
)

// Go spawns fn in a new goroutine. The goroutine name is auto-derived from
// the calling function's name (via runtime.Caller); any panic is recovered
// and logged with a stack trace, with the original ctx attached to the
// slog record.
//
// Prefer this over `go func() { ... }()` for any goroutine whose lifetime
// outlives a single function call.
func Go(ctx context.Context, fn func(context.Context)) {
	name := fname.CallerFuncName(1)
	go runWithRecover(ctx, name, fn)
}

// GoNamed spawns fn with an explicit name. Use this for goroutines whose
// caller-name would be ambiguous, e.g. a loop that fans out N workers.
func GoNamed(ctx context.Context, name string, fn func(context.Context)) {
	go runWithRecover(ctx, name, fn)
}

func runWithRecover(ctx context.Context, name string, fn func(context.Context)) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "goroutine panic",
				slog.String("name", name),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
		}
	}()
	fn(ctx)
}
