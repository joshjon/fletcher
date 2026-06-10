// Package portmap discovers home-router NAT traversal and asks the
// gateway to forward an external port to the daemon. Used at startup so
// WireGuard / Connect endpoints become reachable from outside the LAN
// without requiring the operator to manually configure the router.
//
// Phase 9 ships UPnP IGD discovery and AddPortMapping. NAT-PMP and PCP
// are documented as follow-ups - the Result shape leaves room for them.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

// Protocol selects TCP vs UDP for the mapping.
type Protocol string

// Protocol values.
const (
	ProtocolTCP Protocol = "TCP"
	ProtocolUDP Protocol = "UDP"
)

// Request is the operator's intent for a port mapping.
type Request struct {
	// InternalPort is the port the daemon is listening on locally.
	InternalPort uint16
	// ExternalPort is the port the gateway should forward from. 0 means
	// "same as InternalPort".
	ExternalPort uint16
	// Protocol selects TCP or UDP.
	Protocol Protocol
	// LeaseDuration is how long the mapping should live; 0 = indefinite
	// (router-dependent - many treat 0 as "until reboot").
	LeaseDuration time.Duration
	// Description is shown in router UIs.
	Description string
}

// Result describes a successful mapping.
type Result struct {
	// Method that produced the mapping. "upnp", later "nat-pmp" / "pcp".
	Method string
	// ExternalIP as observed by the gateway. May be empty if the router
	// returned a private address (LAN-only routers).
	ExternalIP string
	// ExternalPort the gateway is forwarding from.
	ExternalPort uint16
	// InternalIP that the gateway is forwarding to (the host's LAN IP).
	InternalIP string
	// LeaseDuration as accepted by the router. May differ from request.
	LeaseDuration time.Duration
}

// defaultMapLifetime is requested when a Request leaves LeaseDuration
// unset. It is deliberately longer than the refresh interval so a mapping
// outlives a missed refresh, but finite so an abandoned mapping (daemon
// killed without releasing) eventually clears on the router.
const defaultMapLifetime = 2 * time.Hour

// Map attempts to install a port mapping, trying NAT-PMP first and falling
// back to UPnP IGD. NAT-PMP is tried first because it is a single UDP
// round-trip to the gateway and some routers honor it for TCP where their
// UPnP silently does not. The returned error is informational - callers
// should treat absence of NAT punch-through as "operator must configure
// manually", not as a fatal startup failure.
func Map(ctx context.Context, req Request) (Result, error) {
	if req.Protocol != ProtocolTCP && req.Protocol != ProtocolUDP {
		return Result{}, fmt.Errorf("invalid protocol %q", req.Protocol)
	}
	if req.InternalPort == 0 {
		return Result{}, errors.New("InternalPort is required")
	}
	if req.ExternalPort == 0 {
		req.ExternalPort = req.InternalPort
	}
	if req.LeaseDuration == 0 {
		req.LeaseDuration = defaultMapLifetime
	}

	if gw, err := defaultGatewayV4(); err == nil {
		if res, err := mapNATPMP(ctx, req, gw); err == nil {
			return res, nil
		}
	}

	internalIP, err := defaultLANIP(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("detect LAN ip: %w", err)
	}
	return mapUPnP(ctx, req, internalIP)
}

// Unmap removes a previously installed mapping, trying NAT-PMP then UPnP.
// Best-effort: used on shutdown so Fletcher does not leave stale forwards
// on the router. Errors are returned for logging but are not actionable.
func Unmap(ctx context.Context, req Request) error {
	if req.ExternalPort == 0 {
		req.ExternalPort = req.InternalPort
	}
	if gw, err := defaultGatewayV4(); err == nil {
		if err := unmapNATPMP(ctx, req, gw); err == nil {
			return nil
		}
	}
	return unmapUPnP(ctx, req)
}

// unmapUPnP deletes a UPnP IGD mapping by external port + protocol.
func unmapUPnP(ctx context.Context, req Request) error {
	if clients2, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx); err == nil && len(clients2) > 0 {
		return clients2[0].DeletePortMappingCtx(ctx, "", req.ExternalPort, string(req.Protocol))
	}
	clients1, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx)
	if err != nil {
		return fmt.Errorf("upnp discover: %w", err)
	}
	if len(clients1) == 0 {
		return errors.New("no UPnP IGD found on the LAN")
	}
	return clients1[0].DeletePortMappingCtx(ctx, "", req.ExternalPort, string(req.Protocol))
}

func mapUPnP(ctx context.Context, req Request, internalIP string) (Result, error) {
	// Try IGD v2 first (PCP-aware), fall back to v1.
	clients2, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
	if err == nil && len(clients2) > 0 {
		c := clients2[0]
		lease := uint32(req.LeaseDuration.Seconds())
		if err := c.AddPortMappingCtx(ctx,
			"",                   // RemoteHost (empty = any)
			req.ExternalPort,     // ExternalPort
			string(req.Protocol), // PortMappingProtocol
			req.InternalPort,     // InternalPort
			internalIP,           // InternalClient
			true,                 // Enabled
			req.Description,      // PortMappingDescription
			lease,                // PortMappingLeaseDuration
		); err != nil {
			return Result{}, fmt.Errorf("upnp v2 add mapping: %w", err)
		}
		externalIP, _ := c.GetExternalIPAddressCtx(ctx)
		return Result{
			Method:        "upnp",
			ExternalIP:    externalIP,
			ExternalPort:  req.ExternalPort,
			InternalIP:    internalIP,
			LeaseDuration: time.Duration(lease) * time.Second,
		}, nil
	}

	clients1, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("upnp discover: %w", err)
	}
	if len(clients1) == 0 {
		return Result{}, errors.New("no UPnP IGD found on the LAN")
	}
	c := clients1[0]
	lease := uint32(req.LeaseDuration.Seconds())
	if err := c.AddPortMappingCtx(ctx,
		"",
		req.ExternalPort,
		string(req.Protocol),
		req.InternalPort,
		internalIP,
		true,
		req.Description,
		lease,
	); err != nil {
		return Result{}, fmt.Errorf("upnp v1 add mapping: %w", err)
	}
	externalIP, _ := c.GetExternalIPAddressCtx(ctx)
	return Result{
		Method:        "upnp",
		ExternalIP:    externalIP,
		ExternalPort:  req.ExternalPort,
		InternalIP:    internalIP,
		LeaseDuration: time.Duration(lease) * time.Second,
	}, nil
}

// defaultLANIP returns the IPv4 address on the interface that owns the
// default route. We open a UDP socket "to" a public address - that
// resolves the kernel's route choice without sending any packets.
func defaultLANIP(ctx context.Context) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", errors.New("non-UDP local address")
	}
	return addr.IP.String(), nil
}
