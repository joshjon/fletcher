//go:build !linux

// Package ext4driver compiles to a "not supported" stub off Linux. The real
// driver in ext4driver_linux.go clones ext4 image files (and uses the Linux
// FICLONE ioctl), so it only makes sense on Linux.
package ext4driver

import (
	"context"
	"fmt"
	"runtime"

	"github.com/joshjon/fletcher/internal/snapshot"
)

// Driver is the cross-platform shim satisfying snapshot.Driver.
type Driver struct{}

// Options matches the Linux build's surface so call sites need no build guards.
type Options struct {
	RootDir   string
	ImagesDir string
}

// New refuses to construct off Linux; a daemon configured with --snapshot=ext4
// there fails fast with a clear message.
func New(_ Options) (*Driver, error) {
	return nil, fmt.Errorf("ext4 snapshot driver is only supported on Linux (current GOOS=%s)", runtime.GOOS)
}

// Create is unreachable in practice; provided for interface satisfaction.
func (*Driver) Create(context.Context, string) (snapshot.Snapshot, error) {
	return snapshot.Snapshot{}, fmt.Errorf("ext4 snapshot driver not available on %s", runtime.GOOS)
}

// Delete is similarly unreachable but provided for interface compliance.
func (*Driver) Delete(context.Context, string) error {
	return fmt.Errorf("ext4 snapshot driver not available on %s", runtime.GOOS)
}
