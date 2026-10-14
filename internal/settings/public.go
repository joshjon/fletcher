package settings

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// DefaultPublicWebListen is the loopback-only behind-proxy app listener.
const DefaultPublicWebListen = "127.0.0.1:11800"

// ValidatePublicWebListen requires a literal loopback IP and a nonzero TCP port.
func ValidatePublicWebListen(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("expected loopback IP:port: %w", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" {
		return fmt.Errorf("public web listen must use a literal loopback IP")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("public web listen port must be 1-65535")
	}
	return nil
}

// ParsePublicWebTrustedProxies parses space-separated source CIDRs; empty trusts nobody.
func ParsePublicWebTrustedProxies(value string) ([]netip.Prefix, error) {
	fields := strings.Fields(value)
	prefixes := make([]netip.Prefix, 0, len(fields))
	for _, field := range fields {
		prefix, err := netip.ParsePrefix(field)
		if err != nil || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q", field)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func validatePublicWebTrustedProxies(value string) error {
	_, err := ParsePublicWebTrustedProxies(value)
	return err
}
