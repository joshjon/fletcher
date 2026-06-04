//go:build !linux

package wireguard

import (
	"context"
	"errors"
	"log/slog"
)

// ErrTunnelNotSupported is returned when the daemon tries to bring up a
// WireGuard tunnel on a non-Linux platform. Mac development uses the
// mock runtime and skips the tunnel entirely.
var ErrTunnelNotSupported = errors.New("wireguard tunnel not supported on this platform; use macOS for development only, ship on Linux")

// stubTunnel is the non-Linux stand-in: every operation returns
// ErrTunnelNotSupported, so the daemon's coordination code can still
// compile and the operator gets a clear message when they try to use
// it outside Linux.
type stubTunnel struct{}

// NewLinuxTunnel returns a stub on non-Linux platforms so the daemon's
// wiring compiles. Calls to Start error with ErrTunnelNotSupported.
func NewLinuxTunnel(_ *slog.Logger) Tunnel { return stubTunnel{} }

func (stubTunnel) Start(_ context.Context, _ TunnelConfig) error {
	return ErrTunnelNotSupported
}

func (stubTunnel) SetPeers(_ context.Context, _ []PeerConfig) error {
	return ErrTunnelNotSupported
}

func (stubTunnel) Stop() error { return nil }
