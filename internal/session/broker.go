package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// Broker runs the host-side forwarders for published session ports. Each
// published port gets a TCP listener bound to the tunnel IP; an inbound
// connection is dialed through to the matching loopback port inside the
// session's VM via the dialer (Manager.DialPort), which relays it over vsock so
// the VM stays unroutable. This is the preview-proxy pattern (DESIGN.md §5)
// generalised from SSH to an arbitrary port, reachable by paired clients over
// the WireGuard tunnel.
//
// Phase 2 adds a public frontend (a single 443 listener with TLS + hostname
// routing) on top of the same dialer; the tunnel forwarders here always work.
type Broker struct {
	// tunnelIP is the host IP published-port listeners bind on (the WireGuard
	// server tunnel IP). Empty disables the broker: published ports are recorded
	// but unreachable until the tunnel is up.
	tunnelIP string
	dial     PortDialer
	logger   *slog.Logger
	// ctx bounds dials for in-flight connections; cancelled when the daemon
	// shuts down.
	ctx context.Context //nolint:containedctx // broker lifetime is the daemon's; used to bound per-connection dials

	mu        sync.Mutex
	listeners map[string]net.Listener // published-port id -> listener
}

// PortDialer opens a stream to a loopback port inside a session's VM. Satisfied
// by Manager.DialPort.
type PortDialer func(ctx context.Context, sessionID string, port uint16) (net.Conn, error)

// guestDialTimeout bounds a single inbound connection's dial into the VM. It is
// generous because a connection to a hibernated session wakes it first (a cold
// boot is ~1-2s), and the published service may still be coming up.
const guestDialTimeout = 90 * time.Second

// NewBroker constructs a port broker. tunnelIP is the host IP to bind on (empty
// disables forwarding); dial reaches the guest port over vsock.
func NewBroker(ctx context.Context, tunnelIP string, dial PortDialer, logger *slog.Logger) *Broker {
	return &Broker{
		tunnelIP:  tunnelIP,
		dial:      dial,
		logger:    logger,
		ctx:       ctx,
		listeners: make(map[string]net.Listener),
	}
}

// Open binds the host-side listener for a published port and starts forwarding.
// It reuses pp.TunnelPort when non-zero (so a port keeps the same address across
// restarts), else lets the OS pick a free port, returning the bound port.
func (b *Broker) Open(pp PublishedPort) (int, error) {
	if b.tunnelIP == "" {
		return 0, fmt.Errorf("no tunnel address; published ports need the WireGuard tunnel up")
	}
	addr := net.JoinHostPort(b.tunnelIP, strconv.Itoa(pp.TunnelPort))
	var lc net.ListenConfig
	ln, err := lc.Listen(b.ctx, "tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("listen %s: %w", addr, err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return 0, fmt.Errorf("unexpected listener addr type %T", ln.Addr())
	}

	b.mu.Lock()
	// A stale listener for this id (e.g. republish) must not leak.
	if old := b.listeners[pp.ID]; old != nil {
		_ = old.Close()
	}
	b.listeners[pp.ID] = ln
	b.mu.Unlock()

	go b.serve(ln, pp.SessionID, uint16(pp.GuestPort)) //nolint:gosec // guest port is validated 1..65535 before publish
	b.logger.Info("published port forwarding",
		slog.String("session_id", pp.SessionID),
		slog.Int("guest_port", pp.GuestPort),
		slog.Int("tunnel_port", tcp.Port))
	return tcp.Port, nil
}

// Close stops forwarding the published port with this id.
func (b *Broker) Close(id string) {
	b.mu.Lock()
	ln := b.listeners[id]
	delete(b.listeners, id)
	b.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}

// CloseAll shuts every forwarder down (daemon shutdown).
func (b *Broker) CloseAll() {
	b.mu.Lock()
	lns := make([]net.Listener, 0, len(b.listeners))
	for id, ln := range b.listeners {
		lns = append(lns, ln)
		delete(b.listeners, id)
	}
	b.mu.Unlock()
	for _, ln := range lns {
		_ = ln.Close()
	}
}

// serve accepts connections on a published port's listener and forwards each to
// the guest port. It returns when the listener is closed.
func (b *Broker) serve(ln net.Listener, sessionID string, guestPort uint16) {
	for {
		client, err := ln.Accept()
		if err != nil {
			return // listener closed (unpublish / shutdown)
		}
		go b.forward(client, sessionID, guestPort)
	}
}

// forward dials the guest port (waking a stopped session) and splices the two
// connections. The dialer keeps the session busy for the connection's lifetime.
func (b *Broker) forward(client net.Conn, sessionID string, guestPort uint16) {
	dialCtx, cancel := context.WithTimeout(b.ctx, guestDialTimeout)
	upstream, err := b.dial(dialCtx, sessionID, guestPort)
	cancel()
	if err != nil {
		b.logger.Warn("published port dial guest",
			slog.String("session_id", sessionID),
			slog.Int("guest_port", int(guestPort)),
			slog.String("err", err.Error()))
		_ = client.Close()
		return
	}
	splice(client, upstream)
}

// splice copies bidirectionally between two connections, closing both when
// either direction ends so half-closed streams terminate.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
