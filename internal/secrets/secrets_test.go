package secrets_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/secrets"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

func newStore(t *testing.T) (*secrets.Store, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))

	keyPath := filepath.Join(dir, "age.key")
	store, err := secrets.Open(sqliteq.New(db), keyPath)
	require.NoError(t, err)
	return store, keyPath
}

func TestSetAndGetRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	require.NoError(t, s.Set(ctx, "anthropic_api_key", "sk-ant-secret"))

	got, err := s.Get(ctx, "anthropic_api_key")
	require.NoError(t, err)
	require.Equal(t, "sk-ant-secret", got)
}

func TestGetMissingSecretReturnsNotFound(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Get(context.Background(), "missing")
	require.ErrorIs(t, err, secrets.ErrNotFound)
}

func TestDeleteRemovesSecretAndCache(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	require.NoError(t, s.Set(ctx, "k", "v"))
	require.NoError(t, s.Delete(ctx, "k"))
	_, err := s.Get(ctx, "k")
	require.ErrorIs(t, err, secrets.ErrNotFound)
}

func TestListReturnsMetadataOnlyAndSorted(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	require.NoError(t, s.Set(ctx, "zebra", "v"))
	require.NoError(t, s.Set(ctx, "apple", "v"))

	list, err := s.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "apple", list[0].Name)
	require.Equal(t, "zebra", list[1].Name)
}

func TestOpenAutoGeneratesIdentityWithRestrictedPerms(t *testing.T) {
	_, keyPath := newStore(t)
	info, err := os.Stat(keyPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestOpenLoadsExistingIdentitySoSecretsSurvive(t *testing.T) {
	// Round 1: create store, write a secret.
	dir := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))

	keyPath := filepath.Join(dir, "age.key")
	s1, err := secrets.Open(sqliteq.New(db), keyPath)
	require.NoError(t, err)
	require.NoError(t, s1.Set(context.Background(), "k", "v"))

	// Round 2: reopen with the same identity; secret should decrypt.
	s2, err := secrets.Open(sqliteq.New(db), keyPath)
	require.NoError(t, err)
	got, err := s2.Get(context.Background(), "k")
	require.NoError(t, err)
	require.Equal(t, "v", got)
}

func TestOpenRefusesUnparsableIdentity(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "age.key")
	require.NoError(t, os.WriteFile(keyPath, []byte("not an age identity"), 0o600))

	db, err := sqlite.Open(context.Background(), filepath.Join(dir, "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))

	_, err = secrets.Open(sqliteq.New(db), keyPath)
	require.Error(t, err)
	// Sanity: the message names the path so the operator can recover.
	require.ErrorContains(t, err, keyPath)
}
