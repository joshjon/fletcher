// Package snapshot declares the Driver abstraction over the daemon's
// snapshot/fork layer (btrfs subvolumes in production, plain directories
// for the mock driver). Per DESIGN.md §10, all btrfs calls live behind
// this interface.
package snapshot

import "context"

// Snapshot is the handle to a fork of a base environment.
type Snapshot struct {
	ID   string
	Path string
}

// Driver creates and destroys snapshots. Implementations are responsible
// for whatever bookkeeping their backing storage needs.
type Driver interface {
	// Create returns a fresh snapshot of the named image. The returned
	// Snapshot.Path is a usable filesystem path inside the snapshot.
	Create(ctx context.Context, image string) (Snapshot, error)
	// Delete removes the snapshot referenced by id. Deleting a missing
	// snapshot is a no-op (not an error).
	Delete(ctx context.Context, id string) error
}
