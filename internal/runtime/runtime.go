// Package runtime declares the Driver abstraction over execution backends:
// Firecracker microVMs in production (Linux), runc as a degraded-isolation
// fallback, and a mock driver used during macOS development. Per DESIGN.md
// §10, all KVM/Firecracker calls live behind this interface.
package runtime

import (
	"context"
	"io"
)

// Spec describes a single execution: what to run and the environment it
// should run in. Implementations interpret Image / WorkDir as appropriate
// for their backend (a Firecracker rootfs, a runc bundle, or a plain
// directory for the mock driver).
type Spec struct {
	JobID   string
	Image   string
	Command string
	WorkDir string
	Env     []string // "KEY=value" entries; merged onto the driver's defaults
}

// Result is the outcome of a finished execution. Drivers return a Result
// with ExitCode populated for both success and non-zero exits; a non-nil
// error indicates the driver itself failed (couldn't launch, lost the
// process unexpectedly, etc.), not user-program failure.
type Result struct {
	ExitCode int32
}

// Driver runs a Spec to completion. Implementations must honour ctx
// cancellation by killing the underlying process and returning promptly;
// the returned error in that case should wrap ctx.Err().
type Driver interface {
	Run(ctx context.Context, spec Spec, stdout, stderr io.Writer) (Result, error)
}
