package portmap

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHexToIPv4LE(t *testing.T) {
	// /proc/net/route stores the gateway as little-endian hex: the bytes
	// 01 00 A8 C0 decode to 192.168.0.1.
	ip, err := hexToIPv4LE("0100A8C0")
	require.NoError(t, err)
	require.Equal(t, "192.168.0.1", ip.String())

	ip, err = hexToIPv4LE("0101A8C0")
	require.NoError(t, err)
	require.Equal(t, "192.168.1.1", ip.String())

	_, err = hexToIPv4LE("nothex")
	require.Error(t, err)
}

func TestNatpmpOpcode(t *testing.T) {
	udp, ok := natpmpOpcode(ProtocolUDP)
	require.True(t, ok)
	require.Equal(t, byte(1), udp)

	tcp, ok := natpmpOpcode(ProtocolTCP)
	require.True(t, ok)
	require.Equal(t, byte(2), tcp)

	_, ok = natpmpOpcode(Protocol("sctp"))
	require.False(t, ok)
}
