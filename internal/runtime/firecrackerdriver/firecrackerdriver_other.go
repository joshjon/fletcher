//go:build !linux

// Package firecrackerdriver compiles to a "not supported" stub off Linux. The
// real driver in firecrackerdriver_linux.go boots KVM microVMs and only makes
// sense on Linux.
package firecrackerdriver

import (
	"context"
	"fmt"
	"io"
	"runtime"

	fcruntime "github.com/joshjon/fletcher/internal/runtime"
)

// Driver is the cross-platform shim satisfying runtime.Driver.
type Driver struct{}

// Forward matches the Linux build's surface so call sites need no build guards.
type Forward struct {
	ListenAddr string
	HostSocket string
}

// Options matches the Linux build's surface so call sites need no build guards.
type Options struct {
	FirecrackerBinary string
	KernelPath        string
	RunDir            string
	Forwards          []Forward
	VcpuCount         int64
	MemSizeMib        int64
}

// New refuses to construct off Linux; a daemon configured with
// --runtime=firecracker there fails fast with a clear message.
func New(_ Options) (*Driver, error) {
	return nil, fmt.Errorf("firecracker runtime is only supported on Linux (current GOOS=%s)", runtime.GOOS)
}

// Run is unreachable in practice; provided for interface satisfaction.
func (*Driver) Run(context.Context, fcruntime.Spec, io.Writer, io.Writer) (fcruntime.Result, error) {
	return fcruntime.Result{}, fmt.Errorf("firecracker runtime not available on %s", runtime.GOOS)
}
