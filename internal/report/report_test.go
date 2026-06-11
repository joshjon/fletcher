package report_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/events"
	"github.com/joshjon/fletcher/internal/report"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// recordingSink captures published events.
type recordingSink struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recordingSink) Publish(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func newService(t *testing.T) (*report.Service, *recordingSink) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "fletcher.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	sink := &recordingSink{}
	return report.NewService(sqliteq.New(db), sink), sink
}

func TestCreateStoresAndAnnounces(t *testing.T) {
	svc, sink := newService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, report.CreateParams{
		SourceType: "session",
		SourceID:   "session_1",
		SourceName: "dev",
		Title:      "Web app ready",
		Summary:    "Built and published as webapp.",
		Status:     report.StatusSuccess,
		Link:       "https://app.example.com",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)

	got, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, "Web app ready", got.Title)
	require.Equal(t, report.StatusSuccess, got.Status)
	require.Equal(t, "dev", got.SourceName)

	require.Len(t, sink.events, 1)
	require.Equal(t, events.TypeReport, sink.events[0].Type)
	require.Equal(t, "created", sink.events[0].Action)
	require.Equal(t, created.ID, sink.events[0].ID)
	require.Equal(t, "Web app ready", sink.events[0].Name)

	list, err := svc.List(ctx, 0, 0)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestCreateValidates(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, report.CreateParams{Title: ""})
	require.Error(t, err)

	_, err = svc.Create(ctx, report.CreateParams{Title: "x", Status: "loud"})
	require.Error(t, err)

	// Default status is info.
	created, err := svc.Create(ctx, report.CreateParams{Title: "x"})
	require.NoError(t, err)
	require.Equal(t, report.StatusInfo, created.Status)
}
