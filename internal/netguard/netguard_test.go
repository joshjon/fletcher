package netguard_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/netguard"
)

func TestDisallowedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254",    // cloud metadata (link-local)
		"169.254.0.1",        // link-local
		"fe80::1",            // link-local v6
		"10.0.0.5",           // private
		"172.16.3.4",         // private
		"192.168.1.1",        // private
		"fc00::1", "fd12::1", // ULA (private v6)
		"0.0.0.0", "::", // unspecified
		"224.0.0.1", "ff02::1", // multicast
	}
	for _, s := range blocked {
		require.Truef(t, netguard.DisallowedIP(net.ParseIP(s)), "%s should be blocked", s)
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		require.Falsef(t, netguard.DisallowedIP(net.ParseIP(s)), "%s should be allowed", s)
	}
}

func TestDialControlBlocksAndAllows(t *testing.T) {
	require.Error(t, netguard.DialControl("tcp", "127.0.0.1:80", nil))
	require.Error(t, netguard.DialControl("tcp", "10.0.0.1:443", nil))
	require.NoError(t, netguard.DialControl("tcp", "8.8.8.8:443", nil))
}
