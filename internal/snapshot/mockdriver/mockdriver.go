// Package mockdriver is the mock snapshot driver: it creates plain
// directories under a root path instead of btrfs subvolumes. Per
// DESIGN.md §10, this is a production-code citizen - what powers
// Fletcher on macOS during dev - not a test hack.
package mockdriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/joshjon/fletcher/internal/snapshot"
)

// Driver creates snapshots as fresh directories under rootDir.
type Driver struct {
	rootDir string
	counter atomic.Uint64
}

// New constructs a Driver that places snapshots under rootDir. rootDir is
// created if it does not exist.
func New(rootDir string) (*Driver, error) {
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot root: %w", err)
	}
	return &Driver{rootDir: rootDir}, nil
}

// Create returns a new directory snapshot. The image string is stored as a
// marker file (real drivers would unpack it).
func (d *Driver) Create(_ context.Context, image string) (snapshot.Snapshot, error) {
	id := fmt.Sprintf("snap-%d-%d", time.Now().UnixNano(), d.counter.Add(1))
	path := filepath.Join(d.rootDir, id)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("create snapshot dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(path, ".image"), []byte(image), 0o600); err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("write image marker: %w", err)
	}
	return snapshot.Snapshot{ID: id, Path: path}, nil
}

// Delete removes the snapshot directory. Missing snapshots are silently
// ignored.
func (d *Driver) Delete(_ context.Context, id string) error {
	path := filepath.Join(d.rootDir, id)
	if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove snapshot %s: %w", id, err)
	}
	return nil
}
