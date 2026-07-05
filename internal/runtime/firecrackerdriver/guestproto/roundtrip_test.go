package guestproto_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/runtime/firecrackerdriver/guestproto"
)

func TestDirListingRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		listing guestproto.DirListing
	}{
		{
			name: "entries with metadata",
			listing: guestproto.DirListing{
				Path: "/home/fletcher",
				Entries: []guestproto.DirEntry{
					{Name: "notes.txt", Size: 42, Mode: 0o644, ModTime: 1700000000},
					{Name: "src", IsDir: true, Mode: 0o755},
					{Name: "link", IsSymlink: true, SymlinkTarget: "/etc/hosts"},
				},
				Truncated: true,
			},
		},
		{
			name:    "error reply",
			listing: guestproto.DirListing{Error: "not a directory"},
		},
		{
			name:    "zero value",
			listing: guestproto.DirListing{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, guestproto.WriteDirListing(&buf, tt.listing))
			got, err := guestproto.ReadDirListing(&buf)
			require.NoError(t, err)
			require.Equal(t, tt.listing, got)
		})
	}
}

func TestFileResultRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		result guestproto.FileResult
	}{
		{
			name:   "read reply",
			result: guestproto.FileResult{Size: 1024, Mode: 0o600},
		},
		{
			name:   "write reply",
			result: guestproto.FileResult{BytesWritten: 512, Sha256: "abc123"},
		},
		{
			name:   "error reply",
			result: guestproto.FileResult{Error: "permission denied"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, guestproto.WriteFileResult(&buf, tt.result))
			got, err := guestproto.ReadFileResult(&buf)
			require.NoError(t, err)
			require.Equal(t, tt.result, got)
		})
	}
}

func TestStatRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := guestproto.Stat{Load1: 0.42, AppRestarts: 3}
	require.NoError(t, guestproto.WriteStat(&buf, want))
	got, err := guestproto.ReadStat(&buf)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestDialPortRoundTrip(t *testing.T) {
	for _, port := range []uint16{0, 22, 8080, 65535} {
		var buf bytes.Buffer
		require.NoError(t, guestproto.WriteDialPort(&buf, port))
		got, err := guestproto.ReadDialPort(&buf)
		require.NoError(t, err)
		require.Equal(t, port, got)
	}
}

func TestDialPortShortRead(t *testing.T) {
	_, err := guestproto.ReadDialPort(bytes.NewReader([]byte{0x01}))
	require.Error(t, err)
}

func TestRequestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		req  guestproto.Request
	}{
		{
			name: "exec with a full spec",
			req: guestproto.Request{
				Kind: guestproto.RequestExec,
				Spec: guestproto.Spec{
					Command: "make check",
					Env:     []string{"CI=1"},
					AppEnv:  []string{"PORT=8080"},
					WorkDir: "/workspace",
					Forwards: []guestproto.Forward{
						{ListenAddr: "127.0.0.1:11500", VsockPort: guestproto.ForwardPortBase},
					},
					Credentials: []guestproto.CredentialFile{
						{Path: "/home/fletcher/.claude/token", Mode: 0o600, Data: []byte("secret")},
					},
				},
			},
		},
		{
			name: "shell with a pty spec",
			req: guestproto.Request{
				Kind: guestproto.RequestShell,
				Shell: guestproto.ShellSpec{
					Term: "xterm-256color", Cols: 120, Rows: 40,
					Env:         []string{"LANG=C.UTF-8"},
					ControlMode: true,
				},
			},
		},
		{
			name: "write file",
			req: guestproto.Request{
				Kind: guestproto.RequestWriteFile,
				File: guestproto.FileSpec{Path: "notes.txt", Mode: 0o644, Size: 42, Overwrite: true},
			},
		},
		{
			name: "file op",
			req: guestproto.Request{
				Kind:   guestproto.RequestFileOp,
				FileOp: guestproto.FileOpSpec{Op: guestproto.FileOpCopy, Path: "/a", Dest: "/b", Recursive: true},
			},
		},
		{
			name: "shutdown carries no payload",
			req:  guestproto.Request{Kind: guestproto.RequestShutdown},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, guestproto.WriteRequest(&buf, tt.req))
			got, err := guestproto.ReadRequest(&buf)
			require.NoError(t, err)
			require.Equal(t, tt.req, got)
		})
	}
}

// A generic ErrorResponse must surface through any typed reply reader, so an
// older or confused guest still produces a readable error instead of a
// mysterious decode failure.
func TestWriteErrorSurfacesThroughTypedReaders(t *testing.T) {
	t.Run("as a file result", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, guestproto.WriteError(&buf, "unknown request kind"))
		got, err := guestproto.ReadFileResult(&buf)
		require.NoError(t, err)
		require.Equal(t, "unknown request kind", got.Error)
	})

	t.Run("as a dir listing", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, guestproto.WriteError(&buf, "unknown request kind"))
		got, err := guestproto.ReadDirListing(&buf)
		require.NoError(t, err)
		require.Equal(t, "unknown request kind", got.Error)
	})
}

func TestResizeCodec(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		for _, tc := range []struct{ cols, rows uint16 }{
			{80, 24}, {0, 0}, {65535, 65535}, {1, 9999},
		} {
			cols, rows, err := guestproto.DecodeResize(guestproto.EncodeResize(tc.cols, tc.rows))
			require.NoError(t, err)
			require.Equal(t, tc.cols, cols)
			require.Equal(t, tc.rows, rows)
		}
	})

	t.Run("wrong payload length is rejected", func(t *testing.T) {
		for _, payload := range [][]byte{nil, {1}, {1, 2, 3}, {1, 2, 3, 4, 5}} {
			_, _, err := guestproto.DecodeResize(payload)
			require.Error(t, err)
		}
	})
}

func TestExitDecodeNegativeCode(t *testing.T) {
	// The codec deliberately reinterprets int32 <-> uint32, so a negative code
	// (a killed process, by some conventions) survives the trip.
	got, err := guestproto.DecodeExit(guestproto.EncodeExit(-1))
	require.NoError(t, err)
	require.Equal(t, int32(-1), got)
}

// A corrupt length prefix beyond the 16 MiB cap must be rejected before any
// allocation, for both the JSON message reader and the frame reader.
func TestOversizedLengthPrefixRejected(t *testing.T) {
	t.Run("json message", func(t *testing.T) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 17<<20)
		_, err := guestproto.ReadRequest(bytes.NewReader(hdr[:]))
		require.Error(t, err)
		require.ErrorContains(t, err, "message too large")
	})

	t.Run("frame", func(t *testing.T) {
		hdr := []byte{guestproto.KindStdout, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(hdr[1:], 17<<20)
		_, _, err := guestproto.ReadFrame(bytes.NewReader(hdr))
		require.Error(t, err)
		require.ErrorContains(t, err, "frame too large")
	})
}
