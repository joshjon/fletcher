package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/sqlite"
)

func TestOpenAndMigrateRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fletcher.db")
	ctx := context.Background()

	db, err := sqlite.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, sqlite.Migrate(db))

	// Migrate again to verify ErrNoChange is treated as success.
	require.NoError(t, sqlite.Migrate(db))

	// Pragmas should be applied - check journal_mode is WAL.
	var mode string
	require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&mode))
	require.Equal(t, "wal", mode)

	// Foreign keys should be enabled.
	var fk int
	require.NoError(t, db.QueryRow("PRAGMA foreign_keys").Scan(&fk))
	require.Equal(t, 1, fk)
}
