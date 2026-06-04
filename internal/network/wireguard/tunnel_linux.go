//go:build linux

package wireguard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/vishvananda/netlink"
	wgconn "golang.zx2c4.com/wireguard/conn"
	wgdevice "golang.zx2c4.com/wireguard/device"
	wgtun "golang.zx2c4.com/wireguard/tun"
)

// LinuxTunnel is the production Tunnel implementation: wireguard-go's
// userspace device backed by a TUN interface created via netlink. The
// daemon must run with CAP_NET_ADMIN to create/configure the interface
// (the systemd unit grants this via AmbientCapabilities).
type LinuxTunnel struct {
	logger *slog.Logger

	mu       sync.Mutex
	dev      *wgdevice.Device
	tun      wgtun.Device
	linkName string
	cfg      TunnelConfig
}

// NewLinuxTunnel constructs an unstarted Tunnel backed by wireguard-go +
// netlink. Returns Tunnel (not *LinuxTunnel) so the call site matches
// the non-Linux build's signature.
func NewLinuxTunnel(logger *slog.Logger) Tunnel {
	if logger == nil {
		logger = slog.Default()
	}
	return &LinuxTunnel{logger: logger}
}

// Start creates the TUN interface, configures wireguard-go with the
// server private key, listen port, and peers, then assigns the tunnel
// address via netlink and brings the link up.
func (t *LinuxTunnel) Start(_ context.Context, cfg TunnelConfig) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dev != nil {
		return errors.New("tunnel already started")
	}
	if cfg.InterfaceName == "" {
		cfg.InterfaceName = "fletcher0"
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1420
	}
	if cfg.PrivateKey == "" {
		return errors.New("PrivateKey is required")
	}
	if cfg.ListenPort <= 0 {
		return errors.New("ListenPort is required")
	}
	if cfg.Address == "" {
		return errors.New("Address is required")
	}

	tdev, err := wgtun.CreateTUN(cfg.InterfaceName, cfg.MTU)
	if err != nil {
		return fmt.Errorf("create tun %q: %w", cfg.InterfaceName, err)
	}
	realName, err := tdev.Name()
	if err != nil {
		_ = tdev.Close()
		return fmt.Errorf("read tun name: %w", err)
	}

	logger := &wgdevice.Logger{
		Verbosef: func(format string, args ...any) { t.logger.Debug(fmt.Sprintf(format, args...)) },
		Errorf:   func(format string, args ...any) { t.logger.Error(fmt.Sprintf(format, args...)) },
	}
	dev := wgdevice.NewDevice(tdev, wgconn.NewDefaultBind(), logger)

	uapi, err := uapiConfig(cfg.PrivateKey, cfg.ListenPort, cfg.Peers)
	if err != nil {
		dev.Close()
		return fmt.Errorf("render uapi config: %w", err)
	}
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return fmt.Errorf("apply uapi config: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return fmt.Errorf("bring device up: %w", err)
	}

	if err := configureLink(realName, cfg.Address, cfg.MTU); err != nil {
		dev.Close()
		return fmt.Errorf("configure link %q: %w", realName, err)
	}

	t.dev = dev
	t.tun = tdev
	t.linkName = realName
	t.cfg = cfg
	t.logger.Info("wireguard tunnel up",
		slog.String("interface", realName),
		slog.String("address", cfg.Address),
		slog.Int("listen_port", cfg.ListenPort),
		slog.Int("peers", len(cfg.Peers)),
	)
	return nil
}

// SetPeers replaces the running tunnel's peer set without restarting.
// Safe to call from any goroutine.
func (t *LinuxTunnel) SetPeers(_ context.Context, peers []PeerConfig) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dev == nil {
		return errors.New("tunnel not started")
	}
	uapi, err := uapiConfig(t.cfg.PrivateKey, t.cfg.ListenPort, peers)
	if err != nil {
		return fmt.Errorf("render uapi config: %w", err)
	}
	if err := t.dev.IpcSet(uapi); err != nil {
		return fmt.Errorf("apply uapi config: %w", err)
	}
	t.cfg.Peers = peers
	t.logger.Info("wireguard peer set updated", slog.Int("peers", len(peers)))
	return nil
}

// Stop tears down the interface.
func (t *LinuxTunnel) Stop() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dev == nil {
		return nil
	}
	t.dev.Close()
	t.dev = nil
	t.tun = nil
	t.logger.Info("wireguard tunnel stopped", slog.String("interface", t.linkName))
	t.linkName = ""
	return nil
}

// configureLink assigns the tunnel address to the named interface and
// brings it up via netlink. The MTU is applied here too (wireguard-go
// sets it on the TUN device, netlink sets it on the link layer).
func configureLink(name, addrCIDR string, mtu int) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("look up link: %w", err)
	}
	addr, err := netlink.ParseAddr(addrCIDR)
	if err != nil {
		return fmt.Errorf("parse address %q: %w", addrCIDR, err)
	}
	// AddrReplace is idempotent (Add returns EEXIST on rerun).
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("set address: %w", err)
	}
	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return fmt.Errorf("set mtu: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	return nil
}

// ensure the linter doesn't complain about the unused stdlib import on
// build configurations that strip it.
var _ = net.IPv4zero
