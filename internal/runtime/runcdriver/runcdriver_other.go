//go:build !linux

// Package runcdriver compiles to a "not supported" stub on non-Linux
// platforms. The real driver in runcdriver_linux.go invokes the Linux
// runc binary.
package runcdriver

import (
	"context"
	"fmt"
	"io"
	"runtime"

	fruntime "github.com/joshjon/fletcher/internal/runtime"
)

// Driver is the cross-platform shim.
type Driver struct{}

// Options matches the Linux surface so call sites work without build tags.
type Options struct {
	Binary    string
	BundleDir string
}

// New refuses to construct on non-Linux. Daemons configured with
// --runtime=runc on macOS/Windows fail fast at startup.
func New(_ Options) (*Driver, error) {
	return nil, fmt.Errorf("runc runtime driver is only supported on Linux (current GOOS=%s)", runtime.GOOS)
}

// Run is unreachable in practice because New rejects construction;
// implemented for interface-satisfaction.
func (*Driver) Run(context.Context, fruntime.Spec, io.Writer, io.Writer) (fruntime.Result, error) {
	return fruntime.Result{}, fmt.Errorf("runc runtime driver not available on %s", runtime.GOOS)
}
