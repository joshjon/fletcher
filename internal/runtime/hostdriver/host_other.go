//go:build !linux

package hostdriver

import "context"

// New rejects host execution on platforms without the Linux account executor.
func New(string) (*Driver, error) {
	return nil, unavailable("direct host sessions are supported only on the Linux daemon host")
}

// ServeActivated rejects account executor startup on non-Linux platforms.
func ServeActivated(context.Context, string) error {
	return unavailable("the host executor requires Linux and systemd")
}
