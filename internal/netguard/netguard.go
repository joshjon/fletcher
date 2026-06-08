// Package netguard is the single SSRF / LAN guard for all daemon-mediated
// egress. Every path that lets a fork reach the outside world through the
// daemon - the MCP http tools and the Phase B forward-proxy - dials through
// DialControl so a fork can never use the daemon to reach loopback, the
// operator's LAN, link-local, or the cloud-metadata endpoint. The check runs
// against the IP actually being dialed (after DNS), so a hostname that
// resolves to an internal address is still refused (no DNS-rebinding bypass).
package netguard

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrBlocked is wrapped by DialControl when a dial is refused by the guard, so
// callers can distinguish a policy/guard denial from an ordinary dial failure
// (errors.Is(err, netguard.ErrBlocked)) - e.g. for accurate audit logging.
var ErrBlocked = errors.New("egress blocked: loopback, link-local, private, or metadata address")

// DisallowedIP reports whether ip is an address daemon-mediated egress must
// never reach: loopback, link-local (which includes the 169.254.169.254 cloud
// metadata endpoint), private (RFC1918 / ULA), unspecified, or multicast. Only
// globally-routable unicast addresses are allowed out.
func DisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		ip.IsPrivate()
}

// DialControl is a net.Dialer Control hook that refuses to connect to any
// disallowed address. Assign it to net.Dialer.Control so the check fires for
// every dial, checked against the resolved IP rather than the hostname.
func DialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("egress: bad dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("egress: cannot parse dial address %q", host)
	}
	if DisallowedIP(ip) {
		return fmt.Errorf("egress to %s: %w", ip, ErrBlocked)
	}
	return nil
}
