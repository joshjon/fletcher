//go:build linux

package doctor

import (
	"fmt"

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
		// A default route has a zero destination prefix.
		if r.Dst != nil {
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
