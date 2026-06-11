package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/push"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

type fakeSender struct {
	sent []string
	gone map[string]bool
}

func (f *fakeSender) Send(_ context.Context, token string, _ push.Notification) (push.SendResult, error) {
	f.sent = append(f.sent, token)
	return push.SendResult{Gone: f.gone[token]}, nil
}

// The notifier pushes to every registered device and drops the ones APNs
// reports dead.
func TestApprovalNotifierPushesAndDropsDeadTokens(t *testing.T) {
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	store := deviceTokenStore{q: sqliteq.New(db)}

	ctx := context.Background()
	require.NoError(t, store.RegisterToken(ctx, "good"))
	require.NoError(t, store.RegisterToken(ctx, "dead"))

	sender := &fakeSender{gone: map[string]bool{"dead": true}}
	n := approvalNotifier{store: store, sender: sender, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	n.push("approval_1")

	require.ElementsMatch(t, []string{"good", "dead"}, sender.sent, "pushed to both tokens")

	remaining, err := store.list(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"good"}, remaining, "the dead token is dropped")
}

// With no sender (APNs unconfigured) the notifier is a no-op, not a panic.
func TestNotifyApprovalCreatedNoSenderIsNoop(t *testing.T) {
	require.NotPanics(t, func() {
		approvalNotifier{}.NotifyApprovalCreated(context.Background(), "x")
	})
}
