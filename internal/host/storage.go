package host

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Category measures allocated blocks and logical bytes without claiming exclusive CoW usage.
type Category struct {
	Name                      string
	Allocated, Logical, Files uint64
}

// Storage is the filesystem's actual capacity and a non-additive category breakdown.
type Storage struct {
	Total, Available uint64
	Categories       []Category
}

// MeasureStorage scans only daemon-owned storage, never following symbolic links.
func MeasureStorage(ctx context.Context, snapshotRoot, stateRoot string) (Storage, error) {
	total, available, err := capacity(snapshotRoot)
	if err != nil {
		return Storage{}, err
	}
	groups := []Category{{Name: "Images"}, {Name: "Session and job forks"}, {Name: "Volumes"}, {Name: "Build cache"}, {Name: "Other snapshot data"}, {Name: "Daemon state"}}
	err = filepath.WalkDir(snapshotRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(snapshotRoot, path)
		if err != nil {
			return err
		}
		index := 4
		switch {
		case strings.HasPrefix(rel, "images"+string(filepath.Separator)):
			index = 0
		case strings.HasPrefix(rel, "volumes"+string(filepath.Separator)), strings.HasPrefix(rel, "vol_"):
			index = 2
		case rel == "buildcache.ext4":
			index = 3
		case strings.HasPrefix(rel, "snap-"):
			index = 1
		}
		return measureFile(d, &groups[index])
	})
	if err != nil {
		return Storage{}, err
	}
	err = filepath.WalkDir(stateRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if filepath.Clean(path) == filepath.Clean(snapshotRoot) {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return measureFile(d, &groups[5])
	})
	return Storage{Total: total, Available: available, Categories: groups}, err
}

func measureFile(d fs.DirEntry, group *Category) error {
	info, err := d.Info()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	group.Files++
	group.Logical += uint64(max(info.Size(), 0))
	group.Allocated += allocated(info)
	return nil
}
