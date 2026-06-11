package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/joshjon/fletcher/internal/events"
	fletchermcp "github.com/joshjon/fletcher/internal/mcp"
	"github.com/joshjon/fletcher/internal/push"
	"github.com/joshjon/fletcher/internal/report"
	"github.com/joshjon/fletcher/internal/session"
	"github.com/joshjon/fletcher/internal/settings"
)

// notifyRouter turns bus events into push notifications. Each event type is
// gated by its notify_* setting, read per event so toggles apply live without
// a restart. Approval pushes stay on their own path (approvalNotifier): they
// fire from inside the approval write and carry the deep-link payload the
// iOS app already depends on.
type notifyRouter struct {
	bus      *events.Bus
	store    deviceTokenStore
	sender   apnsSender
	settings *settings.Store
	reports  *report.Service
	logger   *slog.Logger
}

// run consumes the bus until ctx is cancelled. Without an APNs sender it just
// blocks: returning would collapse the whole run group.
func (r notifyRouter) run(ctx context.Context) {
	if r.sender == nil {
		<-ctx.Done()
		return
	}
	ch, cancel := r.bus.Subscribe()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			r.route(ctx, e)
		}
	}
}

func (r notifyRouter) route(ctx context.Context, e events.Event) {
	display := e.Name
	if display == "" {
		display = e.ID
	}
	var notif push.Notification
	switch {
	case e.Type == events.TypeReport && e.Action == "created":
		if !r.enabled(ctx, settings.KeyNotifyReports) {
			return
		}
		notif = r.reportNotification(ctx, e)
	case e.Type == events.TypeJob && (e.Action == "succeeded" || e.Action == "failed"):
		if !r.enabled(ctx, settings.KeyNotifyJobs) {
			return
		}
		notif = push.Notification{
			Title: "Job finished",
			Body:  fmt.Sprintf("Job %q %s.", display, e.Action),
			Data:  map[string]string{"job_id": e.ID},
		}
	case e.Type == events.TypeSession && e.Action == "idle-stopped":
		if !r.enabled(ctx, settings.KeyNotifySessionIdle) {
			return
		}
		notif = push.Notification{
			Title: "Session finished",
			Body:  fmt.Sprintf("Session %q finished its work and was hibernated.", display),
			Data:  map[string]string{"session_id": e.ID},
		}
	case e.Type == events.TypeSession && e.Action == "crash-looping":
		if !r.enabled(ctx, settings.KeyNotifyDeployHealth) {
			return
		}
		notif = push.Notification{
			Title: "Deploy unhealthy",
			Body:  fmt.Sprintf("Deploy %q is crash-looping.", display),
			Data:  map[string]string{"session_id": e.ID},
		}
	default:
		return
	}
	pushToAll(ctx, r.store, r.sender, r.logger, notif)
}

// reportNotification renders a report event as push content, falling back to
// the event payload when the row cannot be read.
func (r notifyRouter) reportNotification(ctx context.Context, e events.Event) push.Notification {
	title, body := "Fletcher report", e.Name
	if rep, err := r.reports.Get(ctx, e.ID); err == nil {
		title = rep.Title
		body = rep.Summary
		if body == "" && rep.SourceName != "" {
			body = "From " + rep.SourceName + "."
		}
	}
	if body == "" {
		body = "Open Fletcher for detail."
	}
	return push.Notification{
		Title: title,
		Body:  body,
		Data:  map[string]string{"report_id": e.ID},
	}
}

// enabled reads a notify_* toggle, defaulting to on when unset or unreadable.
func (r notifyRouter) enabled(ctx context.Context, key string) bool {
	values, err := r.settings.Values(ctx)
	if err != nil {
		return true
	}
	return values[key] != "false"
}

// pushToAll sends one notification to every registered device, dropping
// tokens APNs reports dead. Shared by the approval notifier and the router.
func pushToAll(ctx context.Context, store deviceTokenStore, sender apnsSender, logger *slog.Logger, notif push.Notification) {
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	tokens, err := store.list(sendCtx)
	if err != nil {
		logger.Warn("push: list device tokens", slog.String("err", err.Error()))
		return
	}
	for _, t := range tokens {
		res, serr := sender.Send(sendCtx, t, notif)
		if serr != nil {
			logger.Warn("push: send", slog.String("err", serr.Error()))
			continue
		}
		if res.Gone {
			if derr := store.delete(sendCtx, t); derr != nil {
				logger.Warn("push: drop dead token", slog.String("err", derr.Error()))
			} else {
				logger.Info("push: dropped a dead device token")
			}
		}
	}
}

// reportPublisher adapts the report service to the mcp.ReportSink the report
// tool needs, resolving the agent-claimed session ref to a real attribution.
type reportPublisher struct {
	reports  *report.Service
	sessions *session.Manager
}

func (p *reportPublisher) CreateReport(ctx context.Context, r fletchermcp.Report) (string, error) {
	params := report.CreateParams{
		Title:   r.Title,
		Summary: r.Summary,
		Status:  r.Status,
		Link:    r.Link,
	}
	if r.SessionRef != "" {
		// Attribution is best-effort: a bad ref leaves the report unattributed
		// rather than rejecting the result it carries.
		if sess, err := p.sessions.Get(ctx, r.SessionRef); err == nil {
			params.SourceType = "session"
			params.SourceID = sess.ID
			params.SourceName = sess.Name
		}
	}
	created, err := p.reports.Create(ctx, params)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}
