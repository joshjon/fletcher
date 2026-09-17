package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRestartIsGenerationGuardedAndIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var restarts atomic.Int32
		m := NewManager(t.Context(), Options{StartedAt: 42, Restart: func() { restarts.Add(1) }})
		m.support = func(context.Context) (bool, string) { return true, "" }
		require.ErrorContains(t, m.RequestRestart(t.Context(), "request_1234567890", 41), "generation changed")
		require.NoError(t, m.RequestRestart(t.Context(), "request_1234567890", 42))
		require.NoError(t, m.RequestRestart(t.Context(), "request_1234567890", 42))
		require.ErrorContains(t, m.RequestRestart(t.Context(), "request_0987654321", 42), "already scheduled")
		time.Sleep(2 * time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, restarts.Load())
	})
}

func TestRestartRejectsUnsupportedAndInvalidSettings(t *testing.T) {
	m := NewManager(t.Context(), Options{StartedAt: 42, Restart: func() { t.Error("must not restart") }})
	m.support = func(context.Context) (bool, string) { return false, "not supervised" }
	require.ErrorContains(t, m.RequestRestart(t.Context(), "request_1234567890", 42), "not supervised")
	m.support = func(context.Context) (bool, string) { return true, "" }
	m.Validate = func(context.Context) error { return errors.New("invalid settings") }
	require.ErrorContains(t, m.RequestRestart(t.Context(), "request_1234567890", 42), "invalid settings")
	require.Empty(t, m.restartID)
}

func TestLogsAreBoundedAndReturnTail(t *testing.T) {
	logs := &Logs{}
	_, err := logs.Write([]byte(strings.Repeat("x", 300*1024)))
	require.NoError(t, err)
	require.LessOrEqual(t, len(logs.data), 256*1024)
	_, err = logs.Write([]byte("\nfirst\nlast\n"))
	require.NoError(t, err)
	require.Equal(t, "first\nlast", logs.Tail(2))
}

func TestStorageSkipsSymlinksAndCountsSnapshotRootOnce(t *testing.T) {
	root := t.TempDir()
	snapshots := filepath.Join(root, "snapshots")
	require.NoError(t, os.MkdirAll(filepath.Join(snapshots, "images"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(snapshots, "images", "app.ext4"), []byte("template"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "state.db"), []byte("state"), 0o600))
	require.NoError(t, os.Symlink(root, filepath.Join(snapshots, "escape")))
	st, err := MeasureStorage(t.Context(), snapshots, root)
	require.NoError(t, err)
	require.NotZero(t, st.Total)
	require.EqualValues(t, 8, st.Categories[0].Logical)
	require.EqualValues(t, 5, st.Categories[5].Logical)
	require.EqualValues(t, 1, st.Categories[5].Files)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = MeasureStorage(ctx, snapshots, root)
	require.ErrorIs(t, err, context.Canceled)
}
