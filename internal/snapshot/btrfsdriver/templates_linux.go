//go:build linux

package btrfsdriver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DeleteTemplate removes a named template through the btrfs driver.
func (d *Driver) DeleteTemplate(ctx context.Context, name string) error {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid template name")
	}
	target := filepath.Join(d.imagesDir, name)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("template is not a subvolume directory")
	}
	return d.runBtrfs(ctx, "subvolume", "delete", target)
}
