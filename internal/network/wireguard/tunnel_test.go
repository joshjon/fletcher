package wireguard

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

// keyA/keyB are fixed 32-byte values in the base64 on-wire form, with their
// expected hex transcodings, so the assertions are true goldens.
const (
	// 32 x 0x01
	keyA    = Key("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	keyAHex = "0101010101010101010101010101010101010101010101010101010101010101"
	// The ASCII bytes "12345678901234567890123456789012"
	keyB    = Key("MTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTI=")
	keyBHex = "3132333435363738393031323334353637383930313233343536373839303132"
)

func TestBase64KeyToHex(t *testing.T) {
	tests := []struct {
		name    string
		key     Key
		want    string
		wantErr string
	}{
		{
			name: "all-ones key",
			key:  keyA,
			want: keyAHex,
		},
		{
			name: "ascii digits key",
			key:  keyB,
			want: keyBHex,
		},
		{
			name:    "invalid base64",
			key:     Key("not base64!!"),
			wantErr: "decode base64 key",
		},
		{
			name:    "empty key decodes but is not 32 bytes",
			key:     Key(""),
			wantErr: "must be 32 bytes, got 0",
		},
		{
			name:    "valid base64 of the wrong length",
			key:     Key(base64.StdEncoding.EncodeToString(make([]byte, 16))),
			wantErr: "must be 32 bytes, got 16",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := base64KeyToHex(tt.key)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestUAPIConfig(t *testing.T) {
	t.Run("golden output with two peers", func(t *testing.T) {
		got, err := uapiConfig(keyA, 51820, []PeerConfig{
			{PublicKey: keyB, AllowedIPs: []string{"10.99.0.2/32", "10.99.0.3/32"}},
			{PublicKey: keyA, AllowedIPs: []string{"10.99.0.4/32"}},
		})
		require.NoError(t, err)
		require.Equal(t, "private_key="+keyAHex+"\n"+
			"listen_port=51820\n"+
			"replace_peers=true\n"+
			"public_key="+keyBHex+"\n"+
			"replace_allowed_ips=true\n"+
			"allowed_ip=10.99.0.2/32\n"+
			"allowed_ip=10.99.0.3/32\n"+
			"public_key="+keyAHex+"\n"+
			"replace_allowed_ips=true\n"+
			"allowed_ip=10.99.0.4/32\n", got)
	})

	t.Run("no peers still replaces the peer set", func(t *testing.T) {
		got, err := uapiConfig(keyA, 7, nil)
		require.NoError(t, err)
		require.Equal(t, "private_key="+keyAHex+"\nlisten_port=7\nreplace_peers=true\n", got)
	})

	t.Run("invalid private key", func(t *testing.T) {
		_, err := uapiConfig(Key("nope"), 51820, nil)
		require.Error(t, err)
		require.ErrorContains(t, err, "server private key")
	})

	t.Run("invalid peer public key", func(t *testing.T) {
		_, err := uapiConfig(keyA, 51820, []PeerConfig{{PublicKey: Key("nope")}})
		require.Error(t, err)
		require.ErrorContains(t, err, "peer public key")
	})
}
