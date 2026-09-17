//go:build linux

package ext4driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DeleteTemplate removes a named ext4 template without following symlinks.
func (d *Driver) DeleteTemplate(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("invalid template name")
	}
	err := os.Remove(filepath.Join(d.imagesDir, name+templateExt))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
