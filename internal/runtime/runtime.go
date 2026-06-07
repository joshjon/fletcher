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
	// EgressPolicy gates the fork's outbound network: "none" | "allowlist" |
	// "open" (empty is treated as "allowlist"). Drivers without egress wiring
	// (mock) ignore it.
	EgressPolicy string
	// Mounts are bind-mounts the runtime should set up inside the fork.
	// Used by the job supervisor to surface trusted-credential dirs
	// (DESIGN.md §5 "Credential modes"); drivers without a notion of
	// mounting (e.g. the mock driver) ignore this field.
	Mounts []Mount
}

// Mount is one bind-mount from the host into the fork's filesystem.
type Mount struct {
	// Source is the absolute path on the host to bind-mount.
	Source string
	// Destination is the absolute path inside the fork to mount at.
	Destination string
	// ReadOnly mounts the source read-only when true. Trusted-credential
	// mounts are read-write because some agents refresh tokens in place.
	ReadOnly bool
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
