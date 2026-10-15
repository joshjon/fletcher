package hostdriver

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/runtime"
)

func TestPacketBounds(t *testing.T) {
	var buf bytes.Buffer
	want := packet{Kind: "input", Data: []byte("hello")}
	require.NoError(t, writePacket(&buf, want))
	var got packet
	require.NoError(t, readPacket(&buf, &got))
	require.Equal(t, want, got)
	for _, n := range []uint32{0, maxPacket + 1, ^uint32(0)} {
		t.Run(strconv.FormatUint(uint64(n), 10), func(t *testing.T) {
			buf.Reset()
			require.NoError(t, binary.Write(&buf, binary.BigEndian, n))
			require.Error(t, readPacket(&buf, &got))
		})
	}
	require.Error(t, writePacket(io.Discard, packet{Data: make([]byte, maxPacket)}))
}

func TestHostIdentityAndUnsupportedOperations(t *testing.T) {
	d := &Driver{account: runtime.HostAccount{User: "operator", UID: 1000, Home: "/home/operator"}}
	_, err := d.StartHostSession(t.Context(), runtime.HostSessionSpec{SessionID: "../escape", Account: d.account})
	require.ErrorContains(t, err, "authorized account")
	h := &handle{}
	_, err = h.Exec(t.Context(), runtime.Spec{}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "interactive terminals")
	_, err = h.DialSSH(t.Context())
	require.Error(t, err)
	_, err = h.DialPort(t.Context(), 80)
	require.Error(t, err)
	_, err = h.WriteFile(t.Context(), runtime.FileWriteSpec{}, bytes.NewReader(nil))
	require.Error(t, err)
	require.Error(t, h.ReadFile(t.Context(), "/file", nil, io.Discard))
	require.Error(t, h.FileOp(t.Context(), runtime.FileOpSpec{Op: runtime.FileOpDelete, Path: "/"}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = d.HostAccount(ctx)
	require.Error(t, err)
}
