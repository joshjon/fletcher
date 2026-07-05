//go:build linux

package doctor

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsDefaultDst(t *testing.T) {
	cidr := func(t *testing.T, s string) *net.IPNet {
		t.Helper()
		_, n, err := net.ParseCIDR(s)
		require.NoError(t, err)
		return n
	}

	tests := []struct {
		name string
		dst  *net.IPNet
		want bool
	}{
		{
			name: "nil dst is a default route",
			dst:  nil,
			want: true,
		},
		{
			name: "explicit 0.0.0.0/0 is a default route",
			dst:  &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			want: true,
		},
		{
			name: "parsed 0.0.0.0/0 is a default route",
			dst:  cidr(t, "0.0.0.0/0"),
			want: true,
		},
		{
			// Unspecified IP alone is not enough; the mask must be /0 too.
			name: "0.0.0.0/8 is not a default route",
			dst:  cidr(t, "0.0.0.0/8"),
			want: false,
		},
		{
			name: "10.0.0.0/8 is not a default route",
			dst:  cidr(t, "10.0.0.0/8"),
			want: false,
		},
		{
			name: "a host route is not a default route",
			dst:  cidr(t, "192.168.1.1/32"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isDefaultDst(tt.dst))
		})
	}
}
