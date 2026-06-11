package volume_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
	"github.com/joshjon/fletcher/internal/volume"
)

// fakeProvisioner records provisioned volumes in memory.
type fakeProvisioner struct {
	mu      sync.Mutex
	created map[string]int64 // id -> size
	deleted []string
}

func newFakeProvisioner() *fakeProvisioner { return &fakeProvisioner{created: map[string]int64{}} }

func (f *fakeProvisioner) CreateVolume(_ context.Context, id string, sizeBytes int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created[id] = sizeBytes
	return "/fake/volumes/" + id + ".ext4", nil
}

func (f *fakeProvisioner) DeleteVolume(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.created, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func newManager(t *testing.T) (*volume.Manager, sqliteq.Querier, *fakeProvisioner) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "fletcher.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	q := sqliteq.New(db)
	prov := newFakeProvisioner()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return volume.NewManager(q, prov, logger), q, prov
}

func TestCreateListDelete(t *testing.T) {
	mgr, _, prov := newManager(t)
	ctx := context.Background()

	v, err := mgr.Create(ctx, "data", 0)
	require.NoError(t, err)
	require.Equal(t, "data", v.Name)
	require.EqualValues(t, volume.DefaultSizeBytes, v.SizeBytes)
	require.EqualValues(t, volume.DefaultSizeBytes, prov.created[v.ID])
	require.Empty(t, v.AttachedSession)

	// Names are unique.
	_, err = mgr.Create(ctx, "data", 0)
	require.Error(t, err)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))

	list, err := mgr.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	require.NoError(t, mgr.Delete(ctx, "data"))
	// deleted also holds the duplicate create's rolled-back provisioning.
	require.Contains(t, prov.deleted, v.ID)
	_, err = mgr.Get(ctx, "data")
	require.Error(t, err)
}

func TestCreateRejectsBadNames(t *testing.T) {
	mgr, _, _ := newManager(t)
	for _, bad := range []string{"", "Has Caps", "../x", ".dot", "-dash", "a b"} {
		_, err := mgr.Create(context.Background(), bad, 0)
		require.Error(t, err, "name %q", bad)
	}
}

func TestAttachmentBlocksDeleteAndReattach(t *testing.T) {
	mgr, q, _ := newManager(t)
	ctx := context.Background()

	v, err := mgr.Create(ctx, "data", 0)
	require.NoError(t, err)

	// First resolve succeeds; record the attachment as a session row would.
	id, path, err := mgr.ResolveAttachable(ctx, "data")
	require.NoError(t, err)
	require.Equal(t, v.ID, id)
	require.Equal(t, v.Path, path)
	createSessionRow(t, q, "dev", &id)

	// Attached: a second attach and a delete are both refused.
	_, _, err = mgr.ResolveAttachable(ctx, "data")
	require.Error(t, err)
	require.Equal(t, errs.CategoryConflict, errs.CategoryOf(err))
	err = mgr.Delete(ctx, "data")
	require.Error(t, err)
	require.Contains(t, err.Error(), "attached to session")

	// The volume reports its attachment.
	got, err := mgr.Get(ctx, "data")
	require.NoError(t, err)
	require.Equal(t, "dev", got.AttachedSession)

	// Detach (the session is deleted) and the volume is deletable again.
	_, err = q.DeleteSession(ctx, "session_test")
	require.NoError(t, err)
	require.NoError(t, mgr.Delete(ctx, "data"))
}

func createSessionRow(t *testing.T, q sqliteq.Querier, name string, volumeID *string) {
	t.Helper()
	_, err := q.CreateSession(context.Background(), sqliteq.CreateSessionParams{
		ID:           "session_test",
		Name:         name,
		Image:        "img",
		State:        "stopped",
		ForkID:       fmt.Sprintf("fork_%s", name),
		ForkPath:     "/tmp/fork",
		CreatedAt:    1,
		UpdatedAt:    1,
		EgressPolicy: "allowlist",
		Gateway:      "on",
		VolumeID:     volumeID,
	})
	require.NoError(t, err)
}
