// Package fname is a small helper for retrieving caller function names at
// runtime. background.Go uses it to derive a goroutine name without
// requiring callers to pass one explicitly.
package fname

import (
	"runtime"
	"strings"
)

// CallerFuncName returns the fully-qualified function name of the function
// that called CallerFuncName, walking up `skip` additional frames.
//
//	skip = 0  → the immediate caller of CallerFuncName
//	skip = 1  → that caller's caller (typical for a helper like background.Go
//	            that wants the name of *its* caller)
//	skip = 2  → and so on
//
// Returns "unknown" if the runtime can't resolve the frame.
func CallerFuncName(skip int) string {
	pc, _, _, ok := runtime.Caller(skip + 1)
	if !ok {
		return "unknown"
	}
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "unknown"
	}
	// Go appends "-fm" to method values; strip that bit of cosmetic noise.
	return strings.TrimSuffix(fn.Name(), "-fm")
}

// ShortFuncName returns just the bare function or method name from a full
// runtime path like "github.com/joshjon/fletcher/internal/job.(*Service).Create".
// Drops the import path and the package/type prefix.
func ShortFuncName(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		full = full[i+1:]
	}
	if i := strings.LastIndex(full, "."); i >= 0 {
		return full[i+1:]
	}
	return full
}
