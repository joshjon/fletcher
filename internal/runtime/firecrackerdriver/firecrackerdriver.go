// Package firecrackerdriver is the placeholder for the Firecracker-backed
// runtime driver. The full implementation (OCI image → rootfs via
// firecracker-containerd, VM lifecycle through firecracker-go-sdk, vsock
// guest agent, MMDS-injected env) is large and load-bearing enough that
// it must be built and verified on a real Linux + KVM host before being
// claimed as supported — see DESIGN.md §11 ("open questions").
//
// For now this package returns a clear, operator-friendly error from New
// so the runtime-selection plumbing is testable end-to-end and the seam
// is real, but the daemon refuses to start with --runtime=firecracker
// until the implementation lands. Mocks remain the default; runc is the
// recommended degraded-isolation fallback in the interim.
package firecrackerdriver

import (
	"context"
	"errors"
	"io"

	"github.com/joshjon/fletcher/internal/runtime"
)

// Driver is the (unfinished) Firecracker runtime driver. The type exists
// so the runtime-selection switch in the daemon can reference it.
type Driver struct{}

// Options configures a future Driver. Fields are placeholders documenting
// what the real implementation will need; none are read today.
type Options struct {
	// KernelPath is the path to the uncompressed vmlinux that Firecracker
	// will boot.
	KernelPath string
	// RootfsTemplate is the base rootfs ext4 image cloned via the snapshot
	// driver before each VM boot.
	RootfsTemplate string
	// APISocketDir is the parent directory under which per-VM API sockets
	// are created.
	APISocketDir string
	// FirecrackerBinary is the path to the firecracker binary. Defaults
	// to "firecracker" via $PATH when empty.
	FirecrackerBinary string
}

// errNotImplemented is the public failure mode of this phase's driver.
// It carries a clear next-step for operators who try to enable
// --runtime=firecracker today.
var errNotImplemented = errors.New(
	"firecracker runtime driver is not implemented yet; " +
		"use --runtime=runc on Linux or --runtime=mock for dev " +
		"(see DESIGN.md §11)",
)

// New always errors today. Phase 8 wires the seam; a later phase fills it
// in once the Linux/KVM verification path is established.
func New(_ Options) (*Driver, error) {
	return nil, errNotImplemented
}

// Run satisfies runtime.Driver for interface composition; unreachable in
// practice because New refuses to construct.
func (*Driver) Run(context.Context, runtime.Spec, io.Writer, io.Writer) (runtime.Result, error) {
	return runtime.Result{}, errNotImplemented
}
