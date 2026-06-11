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

// VolumeProvisioner is the optional capability a Driver advertises when it can
// provision persistent volumes: blank filesystems with their own lifecycle,
// attached to a session as a second disk. A distinct lineage from the
// template-clone forks - a volume is never cloned from anything and outlives
// any session it is attached to.
type VolumeProvisioner interface {
	// CreateVolume provisions a blank volume with sizeBytes capacity and
	// returns the host path of its backing storage. Backing files are sparse,
	// so real disk use grows with data.
	CreateVolume(ctx context.Context, id string, sizeBytes int64) (string, error)
	// DeleteVolume removes the volume's backing storage. Missing is a no-op.
	DeleteVolume(ctx context.Context, id string) error
}

// TemplateCommitter is the optional capability a Driver advertises when it can
// commit an existing snapshot back into a named image template - the
// docker-commit analogue for forks. The caller is responsible for quiescing
// whatever is writing to the snapshot (e.g. syncing a running guest) before
// committing; the result is at worst crash-consistent.
type TemplateCommitter interface {
	// CommitTemplate clones the snapshot referenced by id into a template named
	// name, so future jobs/sessions can boot from it. force replaces an
	// existing template of the same name; without it an existing template is a
	// conflict. extraFiles (absolute in-template path -> content) are written
	// into the template's filesystem before it is published (e.g. the app
	// launch spec a deploy boots with); nil writes nothing.
	CommitTemplate(ctx context.Context, id, name string, force bool, extraFiles map[string][]byte) error
}
