package settings

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublicWebSettings(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		valid      bool
	}{
		{KeyPublicWebMode, "edge", true},
		{KeyPublicWebMode, "behind_proxy", true},
		{KeyPublicWebMode, "", false},
		{KeyPublicWebMode, "proxy", false},
		{KeyPublicWebListen, DefaultPublicWebListen, true},
		{KeyPublicWebListen, "[::1]:11800", true},
		{KeyPublicWebListen, "0.0.0.0:11800", false},
		{KeyPublicWebListen, "[::]:11800", false},
		{KeyPublicWebListen, ":11800", false},
		{KeyPublicWebListen, "localhost:11800", false},
		{KeyPublicWebListen, "192.0.2.1:11800", false},
		{KeyPublicWebListen, "127.0.0.1:0", false},
		{KeyPublicWebListen, "127.0.0.1:65536", false},
		{KeyPublicWebListen, "127.0.0.1:http", false},
		{KeyPublicWebListen, "[::1%lo0]:11800", false},
		{KeyPublicWebTrustedProxies, "", true},
		{KeyPublicWebTrustedProxies, "127.0.0.1/32 ::1/128", true},
		{KeyPublicWebTrustedProxies, "localhost", false},
		{KeyPublicWebTrustedProxies, "127.0.0.1", false},
		{KeyPublicWebTrustedProxies, "::ffff:127.0.0.1/128", false},
		{KeyPublicWebTrustedProxies, "127.0.0.1/32 garbage", false},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			err := newStore().Set(t.Context(), tc.key, tc.value)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.True(t, RequiresRestart(tc.key))
		})
	}
}
