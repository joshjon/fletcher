//go:build linux

package ext4driver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestCopySparse(t *testing.T) {
	mixed := make([]byte, 3<<20+123)
	copy(mixed[4096:], "first data")
	copy(mixed[1<<20-2:], "across a chunk boundary")
	mixed[2<<20+7] = 42
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"all zero", make([]byte, 2<<20+123)},
		{"mixed with trailing hole", mixed},
		{"partial data block", []byte("small disk")},
		{"nonzero", bytes.Repeat([]byte{42}, 1<<20+17)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disk.ext4")
			out, err := os.Create(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = out.Close() })
			require.NoError(t, copySparse(context.Background(), out, bytes.NewReader(tt.data)))
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, len(tt.data), len(got))
			require.True(t, bytes.Equal(tt.data, got))
			if tt.name == "mixed with trailing hole" || tt.name == "all zero" {
				var stat unix.Stat_t
				require.NoError(t, unix.Fstat(int(out.Fd()), &stat))
				require.Less(t, stat.Blocks*512, int64(len(tt.data))/2)
			}
		})
	}
}

func TestCopySparseErrors(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "disk")
	require.NoError(t, err)
	t.Cleanup(func() { _ = out.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, copySparse(ctx, out, bytes.NewReader([]byte("data"))), context.Canceled)
	require.ErrorIs(t, copySparse(context.Background(), out, failingReader{}), io.ErrClosedPipe)
	require.NoError(t, out.Close())
	require.Error(t, copySparse(context.Background(), out, bytes.NewReader([]byte("data"))))
	require.Error(t, copySparse(context.Background(), out, bytes.NewReader(make([]byte, 4096))))
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCloneFileCancellationRemovesDestination(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "destination")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, cloneFile(ctx, src, dst), context.Canceled)
	_, err := os.Stat(dst)
	require.True(t, errors.Is(err, os.ErrNotExist))
}
