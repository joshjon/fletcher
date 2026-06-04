//go:build linux

package doctor

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// defaultRoutes returns the interface names that hold an IPv4 default
// route on this host. Used by CheckDefaultRoutes to warn about
// multi-NIC setups that can cause asymmetric paths.
func defaultRoutes() ([]string, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("read routing table: %w", err)
	}
	seen := make(map[string]bool)
	out := make([]string, 0, 2)
	for _, r := range routes {
		// A default route has either a nil Dst (older kernels / netlink
		// versions omit the field entirely) or an explicit 0.0.0.0/0
		// (newer versions encode it). Accept both.
		if !isDefaultDst(r.Dst) {
			continue
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil {
			continue
		}
		name := link.Attrs().Name
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

// isDefaultDst returns true for the two encodings of "no destination
// constraint" that an IPv4 default route can show up with from
// netlink: nil, or 0.0.0.0/0.
func isDefaultDst(dst *net.IPNet) bool {
	if dst == nil {
		return true
	}
	if dst.IP.IsUnspecified() {
		ones, _ := dst.Mask.Size()
		return ones == 0
	}
	return false
}
