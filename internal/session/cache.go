package session

import (
	"context"
	"errors"
	"os"

	"github.com/joshjon/fletcher/internal/errs"
)

// ClearBuildCache removes the regenerable cache only while no build owns it.
func (m *Manager) ClearBuildCache(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case m.buildCacheSem <- struct{}{}:
		defer func() { <-m.buildCacheSem }()
	default:
		return errs.New(errs.CategoryConflict, "a build is using the cache; wait for it to finish")
	}
	path := m.buildCachePath()
	if path == "" {
		return errs.New(errs.CategoryFailedPrecondition, "build cache unavailable")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
