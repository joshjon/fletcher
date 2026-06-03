//go:build !linux

// Package btrfsdriver compiles to a "not supported" stub on non-Linux
// platforms. The real driver in btrfsdriver_linux.go shells out to
// /usr/bin/btrfs and only makes sense where btrfs subvolumes exist.
package btrfsdriver

import (
	"context"
	"fmt"
	"runtime"

	"github.com/joshjon/fletcher/internal/snapshot"
)

// Driver is the cross-platform shim. It satisfies snapshot.Driver but
// every method returns an OS-not-supported error.
type Driver struct{}

// Options matches the Linux build's surface so call sites don't need
// build-tag guards. All fields are ignored here.
type Options struct {
	RootDir     string
	ImagesDir   string
	BtrfsBinary string
}

// New refuses to construct on non-Linux. Daemons configured with
// --snapshot=btrfs on macOS / Windows fail fast at startup with a clear
// message.
func New(_ Options) (*Driver, error) {
	return nil, fmt.Errorf("btrfs snapshot driver is only supported on Linux (current GOOS=%s)", runtime.GOOS)
}

// Create is unreachable in practice because New rejects construction;
// implemented for interface-satisfaction.
func (*Driver) Create(context.Context, string) (snapshot.Snapshot, error) {
	return snapshot.Snapshot{}, fmt.Errorf("btrfs snapshot driver not available on %s", runtime.GOOS)
}

// Delete is similarly unreachable but provided for interface compliance.
func (*Driver) Delete(context.Context, string) error {
	return fmt.Errorf("btrfs snapshot driver not available on %s", runtime.GOOS)
}
