package volume_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/errs"
)

func TestReservationBlocksDeleteAndHonorsCancellation(t *testing.T) {
	mgr, q, prov := newManager(t)
	v, err := mgr.Create(t.Context(), "data", 0)
	require.NoError(t, err)
	id, path, release, err := mgr.ReserveAttachment(t.Context(), v.ID)
	require.NoError(t, err)
	require.Equal(t, v.Path, path)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, mgr.Delete(ctx, v.Name), context.Canceled)
	require.Empty(t, prov.deleted)
	_, _, _, err = mgr.ReserveAttachment(ctx, v.Name)
	require.ErrorIs(t, err, context.Canceled)
	createSessionRow(t, q, "dev", &id)
	release()
	err = mgr.Delete(t.Context(), v.Name)
	require.Equal(t, errs.CategoryFailedPrecondition, errs.CategoryOf(err))
	require.Empty(t, prov.deleted)
}

func TestReservationRechecksDeletedVolume(t *testing.T) {
	mgr, _, _ := newManager(t)
	v, err := mgr.Create(t.Context(), "data", 0)
	require.NoError(t, err)
	require.NoError(t, mgr.Delete(t.Context(), v.ID))
	_, _, _, err = mgr.ReserveAttachment(t.Context(), v.ID)
	require.Equal(t, errs.CategoryNotFound, errs.CategoryOf(err))
}
