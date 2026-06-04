//go:build !linux

package doctor

import "errors"

// defaultRoutes returns an error on non-Linux platforms; the doctor's
// CheckDefaultRoutes downgrades that error to a Warn so the rest of
// the diagnostic still runs.
func defaultRoutes() ([]string, error) {
	return nil, errors.New("default-route inspection is supported on Linux only")
}
