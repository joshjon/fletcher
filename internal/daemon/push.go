package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/joshjon/fletcher/internal/push"
	"github.com/joshjon/fletcher/internal/settings"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// deviceTokenStore persists APNs device tokens for the PushService and the
// approval notifier.
type deviceTokenStore struct{ q *sqliteq.Queries }

// RegisterToken records a device token (idempotent).
func (s deviceTokenStore) RegisterToken(ctx context.Context, token string) error {
	return s.q.UpsertDeviceToken(ctx, sqliteq.UpsertDeviceTokenParams{Token: token, CreatedAt: time.Now().Unix()})
}

// UnregisterToken removes a device token, reporting whether it existed.
func (s deviceTokenStore) UnregisterToken(ctx context.Context, token string) (bool, error) {
	n, err := s.q.DeleteDeviceToken(ctx, token)
	return n > 0, err
}

func (s deviceTokenStore) list(ctx context.Context) ([]string, error) {
	return s.q.ListDeviceTokens(ctx)
}

func (s deviceTokenStore) delete(ctx context.Context, token string) error {
	_, err := s.q.DeleteDeviceToken(ctx, token)
	return err
}

// apnsSender is the slice of push.Sender the notifier needs (an interface so
// tests can fake it).
type apnsSender interface {
	Send(ctx context.Context, token string, n push.Notification) (push.SendResult, error)
}

// approvalNotifier pushes a content-light APNs notification to every registered
// device when a pending approval is created, dropping tokens APNs reports dead.
// Gated by the notify_approvals setting (read per push, so the toggle is live).
type approvalNotifier struct {
	store    deviceTokenStore
	sender   apnsSender
	settings *settings.Store
	logger   *slog.Logger
}

// NotifyApprovalCreated fires the push asynchronously so it never blocks the
// approval write or holds the request context.
func (n approvalNotifier) NotifyApprovalCreated(_ context.Context, approvalID string) {
	if n.sender == nil {
		return
	}
	// Deliberately detached from the request context: the push must outlive the
	// approval write (whose ctx is about to be cancelled). pushToAll uses its
	// own bounded context.
	go n.push(approvalID) //nolint:gosec,contextcheck // intentionally request-context-independent; see comment
}

func (n approvalNotifier) push(approvalID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if n.settings != nil {
		if values, err := n.settings.Values(ctx); err == nil && values[settings.KeyNotifyApprovals] == "false" {
			return
		}
	}
	pushToAll(ctx, n.store, n.sender, n.logger, push.Notification{
		Title: "Fletcher",
		Body:  "An action needs your approval.",
		Data:  map[string]string{"approval_id": approvalID},
	})
}

// buildAPNSSender builds an APNs sender from cfg, or nil (logged) when APNs is
// unconfigured or the key cannot be loaded - the daemon still runs, just
// without push.
func buildAPNSSender(cfg Config, logger *slog.Logger) apnsSender {
	if cfg.APNSKeyPath == "" {
		return nil
	}
	sender, err := push.NewSender(push.Config{
		KeyPath: cfg.APNSKeyPath,
		KeyID:   cfg.APNSKeyID,
		TeamID:  cfg.APNSTeamID,
		Topic:   cfg.APNSTopic,
		Sandbox: cfg.APNSSandbox,
	})
	if err != nil {
		logger.Error("apns push disabled: could not build sender", slog.String("err", err.Error()))
		return nil
	}
	logger.Info("apns push enabled", slog.String("topic", cfg.APNSTopic), slog.Bool("sandbox", cfg.APNSSandbox))
	return sender
}
