package image

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/snapshot"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

type deletingDriver struct{ deleted []string }

func (*deletingDriver) Create(context.Context, string) (snapshot.Snapshot, error) {
	return snapshot.Snapshot{}, nil
}
func (*deletingDriver) Delete(context.Context, string) error { return nil }
func (d *deletingDriver) DeleteTemplate(_ context.Context, name string) error {
	d.deleted = append(d.deleted, name)
	return nil
}

func testManager(t *testing.T) (*Manager, *deletingDriver) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)
	require.NoError(t, sqlite.Migrate(db))
	driver := &deletingDriver{}
	m, err := NewManager(t.Context(), sqliteq.New(db), t.TempDir(), "ext4", "base", driver, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(m.Close)
	return m, driver
}

func TestImportSurvivesDisconnectAndDeduplicates(t *testing.T) {
	m, _ := testManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m.importFn = func(ctx context.Context, _ ImportOptions) (ImportResult, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-ctx.Done():
			return ImportResult{}, ctx.Err()
		case <-release:
			return ImportResult{Name: "app"}, nil
		}
	}
	requestCtx, cancel := context.WithCancel(t.Context())
	opts := ImportOptions{Ref: "example.com/app:v1", Name: "app"}
	op, err := m.Start(requestCtx, "request_1234567890", opts, false)
	require.NoError(t, err)
	cancel()
	<-entered
	again, err := m.Start(t.Context(), op.ID, opts, false)
	require.NoError(t, err)
	require.Equal(t, op.ID, again.ID)
	_, err = m.Start(t.Context(), "different_123456789", opts, false)
	require.ErrorContains(t, err, "another image operation")
	_, err = m.Start(t.Context(), op.ID, ImportOptions{Ref: "example.com/other", Name: "other"}, false)
	require.ErrorContains(t, err, "different import")
	close(release)
	m.wg.Wait()
	done, err := m.q.GetImageImport(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, "succeeded", done.State)
	require.EqualValues(t, 1, calls.Load())
}

func TestImportFailureDoesNotPersistSensitiveErrors(t *testing.T) {
	m, _ := testManager(t)
	m.importFn = func(context.Context, ImportOptions) (ImportResult, error) {
		return ImportResult{}, errors.New("private registry credential")
	}
	op, err := m.Start(t.Context(), "request_1234567890", ImportOptions{Ref: "example.com/app", Name: "app", Password: "secret"}, false)
	require.NoError(t, err)
	m.wg.Wait()
	done, err := m.q.GetImageImport(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", done.State)
	require.NotContains(t, done.Error, "private registry credential")
	require.NotContains(t, done.Error, "secret")
}

func TestRestartMarksOutstandingImportsInterrupted(t *testing.T) {
	m, _ := testManager(t)
	require.NoError(t, m.q.CreateImageImport(t.Context(), sqliteq.CreateImageImportParams{ID: "old", Ref: "example.com/app", Name: "app"}))
	require.NoError(t, m.q.InterruptImageImports(t.Context(), 123))
	op, err := m.q.GetImageImport(t.Context(), "old")
	require.NoError(t, err)
	require.Equal(t, "failed", op.State)
	require.Contains(t, op.Error, "restarted")
}

func TestDeleteGuardsReferencesAndNames(t *testing.T) {
	m, driver := testManager(t)
	for _, name := range []string{"../secret", "/tmp/image", "", ".", "..", "a/b", "base"} {
		require.Error(t, m.Delete(t.Context(), name))
	}
	_, err := m.q.CreateSession(t.Context(), sqliteq.CreateSessionParams{ID: "session", Name: "session", Image: "app", State: "stopped", ForkID: "fork", ForkPath: "/fork", EgressPolicy: "none", Gateway: "off", EnvVars: "{}"})
	require.NoError(t, err)
	require.ErrorContains(t, m.Delete(t.Context(), "app"), "referenced")
	_, err = m.q.CreateJob(t.Context(), sqliteq.CreateJobParams{ID: "job", Status: "scheduled", TriggerKind: "cron", Name: "job", Image: "scheduled", Command: "true", EgressPolicy: "none", Gateway: "off", Credentials: "[]"})
	require.NoError(t, err)
	require.ErrorContains(t, m.Delete(t.Context(), "scheduled"), "referenced")
	require.NoError(t, m.Delete(t.Context(), "unused"))
	require.Equal(t, []string{"unused"}, driver.deleted)
}

func TestAtomicImportPublication(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "app.ext4")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o600))
	badBuild := func(_ context.Context, _, tmp string) error {
		require.NoError(t, os.WriteFile(tmp, []byte("partial"), 0o600))
		return errors.New("format interrupted")
	}
	require.Error(t, publishExt4(t.Context(), "", target, true, badBuild))
	old, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "old", string(old))
	goodBuild := func(_ context.Context, _, tmp string) error { return os.WriteFile(tmp, []byte("new"), 0o600) }
	require.Error(t, publishExt4(t.Context(), "", target, false, goodBuild))
	require.NoError(t, publishExt4(t.Context(), "", target, true, goodBuild))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new", string(got))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestUpdateRetryReplaysEvenAfterTemplateDeletion(t *testing.T) {
	m, _ := testManager(t)
	require.NoError(t, WriteMeta(m.dir, "app", TemplateMeta{Source: "example.com/app:v1", Digest: "sha256:old"}))
	m.importFn = func(context.Context, ImportOptions) (ImportResult, error) { return ImportResult{Name: "app"}, nil }
	op, err := m.Start(t.Context(), "request_1234567890", ImportOptions{Name: "app"}, true)
	require.NoError(t, err)
	m.wg.Wait()
	require.NoError(t, RemoveMeta(m.dir, "app"))
	again, err := m.Start(t.Context(), op.ID, ImportOptions{Name: "app"}, true)
	require.NoError(t, err)
	require.Equal(t, "succeeded", again.State)
}

func TestBootFileInjectionRejectsEscapingSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "fletcher"), 0o700))
	target := filepath.Join(outside, "fletcher", "app.json")
	require.NoError(t, os.WriteFile(target, []byte("unchanged"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "etc")))
	require.Error(t, writeRootfsFile(root, "/etc/fletcher/app.json", []byte("overwrite"), 0o644))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "unchanged", string(got))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "usr", "sbin"), 0o700))
	require.NoError(t, os.Symlink("usr/sbin", filepath.Join(root, "sbin")))
	require.NoError(t, writeRootfsFile(root, "/sbin/fletcher-init", []byte("init"), 0o755))
	require.FileExists(t, filepath.Join(root, "usr", "sbin", "fletcher-init"))
}

func TestImportRejectsTraversalBeforeRegistryAccess(t *testing.T) {
	_, err := ImportRegistry(t.Context(), ImportOptions{Ref: "example.com/app", Name: "../escape"})
	require.ErrorContains(t, err, "invalid image name")
}
