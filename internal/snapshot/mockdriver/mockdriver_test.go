package mockdriver_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/snapshot/mockdriver"
)

func TestCreateAndDeleteSnapshotRoundTrip(t *testing.T) {
	root := t.TempDir()
	d, err := mockdriver.New(root)
	require.NoError(t, err)

	snap, err := d.Create(context.Background(), "fletcher/ubuntu:24.04")
	require.NoError(t, err)
	require.NotEmpty(t, snap.ID)
	require.DirExists(t, snap.Path)

	// Image marker stored.
	img, err := os.ReadFile(filepath.Join(snap.Path, ".image"))
	require.NoError(t, err)
	require.Equal(t, "fletcher/ubuntu:24.04", string(img))

	require.NoError(t, d.Delete(context.Background(), snap.ID))
	_, err = os.Stat(snap.Path)
	require.True(t, os.IsNotExist(err))
}

func TestDeleteMissingSnapshotIsNoOp(t *testing.T) {
	d, err := mockdriver.New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, d.Delete(context.Background(), "snap-doesnotexist"))
}

func TestCreateSnapshotsHaveDistinctIDs(t *testing.T) {
	d, err := mockdriver.New(t.TempDir())
	require.NoError(t, err)
	seen := make(map[string]struct{})
	for i := 0; i < 5; i++ {
		s, err := d.Create(context.Background(), "x")
		require.NoError(t, err)
		_, dup := seen[s.ID]
		require.False(t, dup, "duplicate snapshot id %q", s.ID)
		seen[s.ID] = struct{}{}
	}
}
