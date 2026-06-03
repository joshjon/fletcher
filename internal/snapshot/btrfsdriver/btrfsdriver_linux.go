//go:build linux

// Package btrfsdriver is the Linux btrfs snapshot driver. It creates each
// snapshot as a btrfs subvolume under a configured root directory (must
// itself live on a btrfs filesystem). When an "image" template subvolume
// is present at <rootDir>/images/<image>, Create takes a CoW snapshot of
// it; otherwise Create makes an empty subvolume so jobs that don't need
// a prepared rootfs still work.
//
// This package only compiles on Linux. The cross-platform shim lives in
// btrfsdriver_other.go and returns "not supported on <GOOS>" at New.
package btrfsdriver

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/joshjon/fletcher/internal/snapshot"
)

// Driver is a snapshot.Driver backed by btrfs subvolumes.
type Driver struct {
	rootDir     string
	imagesDir   string
	btrfsBinary string
	counter     atomic.Uint64
}

// Options configures a Driver.
type Options struct {
	// RootDir is the directory under which snapshots are created. Must
	// live on a btrfs filesystem.
	RootDir string
	// ImagesDir is the directory holding base subvolumes referenced by
	// image name. Defaults to <RootDir>/images.
	ImagesDir string
	// BtrfsBinary is the path to the btrfs CLI. Defaults to "btrfs"
	// (resolved via $PATH).
	BtrfsBinary string
}

// New constructs a Driver. The caller is responsible for ensuring RootDir
// already exists and is on a btrfs filesystem.
func New(opts Options) (*Driver, error) {
	if opts.RootDir == "" {
		return nil, errors.New("btrfs: RootDir is required")
	}
	imagesDir := opts.ImagesDir
	if imagesDir == "" {
		imagesDir = filepath.Join(opts.RootDir, "images")
	}
	binary := opts.BtrfsBinary
	if binary == "" {
		binary = "btrfs"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return nil, fmt.Errorf("btrfs: %s not found in PATH: %w", binary, err)
	}
	return &Driver{
		rootDir:     opts.RootDir,
		imagesDir:   imagesDir,
		btrfsBinary: binary,
	}, nil
}

// Create returns a fresh subvolume for image. If a template subvolume
// exists at <imagesDir>/<image>, a CoW snapshot of it is taken; otherwise
// a new empty subvolume is created.
func (d *Driver) Create(ctx context.Context, image string) (snapshot.Snapshot, error) {
	id := fmt.Sprintf("snap-%d-%d", time.Now().UnixNano(), d.counter.Add(1))
	target := filepath.Join(d.rootDir, id)

	templatePath := ""
	if image != "" {
		templatePath = filepath.Join(d.imagesDir, image)
	}

	var args []string
	if templatePath != "" && fileExists(templatePath) {
		args = []string{"subvolume", "snapshot", templatePath, target}
	} else {
		args = []string{"subvolume", "create", target}
	}
	if err := d.runBtrfs(ctx, args...); err != nil {
		return snapshot.Snapshot{}, fmt.Errorf("create snapshot: %w", err)
	}
	return snapshot.Snapshot{ID: id, Path: target}, nil
}

// Delete removes the subvolume. Missing subvolumes are not an error.
func (d *Driver) Delete(ctx context.Context, id string) error {
	target := filepath.Join(d.rootDir, id)
	if !fileExists(target) {
		return nil
	}
	if err := d.runBtrfs(ctx, "subvolume", "delete", target); err != nil {
		return fmt.Errorf("delete snapshot %s: %w", id, err)
	}
	return nil
}

func (d *Driver) runBtrfs(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, d.btrfsBinary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", d.btrfsBinary, args, err, string(out))
	}
	return nil
}

func fileExists(path string) bool {
	_, err := exec.LookPath(path)
	if err == nil {
		return true
	}
	// LookPath only resolves executables on PATH; fall back to a plain
	// stat for directory / subvolume paths.
	_, err = osStat(path)
	return err == nil
}
