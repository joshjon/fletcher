package job_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/job"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

func newService(t *testing.T) *job.Service {
	return newServiceWithDefaultImage(t, "fletcher-base")
}

func newServiceWithDefaultImage(t *testing.T, defaultImage string) *job.Service {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fletcher.db")
	db, err := sqlite.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	return job.NewService(sqliteq.New(db), nil, defaultImage, "allowlist")
}

func TestCreateAndGetJobRoundTrip(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Name:    "build",
		Command: "make build",
		Image:   "fletcher/go:1.26",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, job.StatusQueued, created.Status)

	got, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created, got)
}

func TestGetMissingJobReturnsNotFound(t *testing.T) {
	svc := newService(t)
	_, err := svc.Get(context.Background(), "job_doesnotexist")
	require.ErrorIs(t, err, job.ErrNotFound)
}

func TestCreateValidatesRequiredFields(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	cases := []struct {
		name string
		p    job.CreateParams
	}{
		{"missing trigger", job.CreateParams{Name: "x", Command: "x", Image: "x"}},
		{"missing command", job.CreateParams{Trigger: job.TriggerEphemeral, Name: "x", Image: "x"}},
		{"invalid trigger", job.CreateParams{Trigger: "nonsense", Name: "x", Command: "x", Image: "x"}},
		{"cron without schedule", job.CreateParams{Trigger: job.TriggerCron, Name: "x", Command: "x", Image: "x"}},
		{"cron with bad schedule", job.CreateParams{Trigger: job.TriggerCron, Name: "x", Command: "x", Image: "x", Schedule: "not a cron"}},
		{"schedule on ephemeral", job.CreateParams{Trigger: job.TriggerEphemeral, Name: "x", Command: "x", Image: "x", Schedule: "* * * * *"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Create(ctx, tc.p)
			require.Error(t, err)
		})
	}
}

func TestCreateDefaultsImage(t *testing.T) {
	svc := newServiceWithDefaultImage(t, "my-base")
	created, err := svc.Create(context.Background(), job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Command: "echo hi",
	})
	require.NoError(t, err)
	require.Equal(t, "my-base", created.Image, "image defaults to the configured default")
}

func TestCreateRequiresImageWhenNoDefault(t *testing.T) {
	svc := newServiceWithDefaultImage(t, "") // no default configured
	_, err := svc.Create(context.Background(), job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Command: "echo hi",
	})
	require.Error(t, err)
}

func TestCreateDefaultsNameFromCommand(t *testing.T) {
	svc := newService(t)
	created, err := svc.Create(context.Background(), job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Command: "claude -p 'say hi'",
		Image:   "fletcher-base",
	})
	require.NoError(t, err)
	require.Equal(t, "claude", created.Name, "name defaults to the command's program name")
}

func TestCreateCronJobIsScheduled(t *testing.T) {
	svc := newService(t)
	created, err := svc.Create(context.Background(), job.CreateParams{
		Trigger:  job.TriggerCron,
		Name:     "hourly-scrape",
		Command:  "scrape.sh",
		Image:    "fletcher-base",
		Schedule: "@hourly",
	})
	require.NoError(t, err)
	require.Equal(t, job.StatusScheduled, created.Status)
	require.Equal(t, "@hourly", created.Schedule)
	require.NotNil(t, created.NextRunAt)
	require.True(t, created.NextRunAt.After(time.Now()), "next run should be in the future")
	require.Nil(t, created.ParentID)
}

func TestListAndCount(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := svc.Create(ctx, job.CreateParams{
			Trigger: job.TriggerEphemeral,
			Name:    "x",
			Command: "x",
			Image:   "x",
		})
		require.NoError(t, err)
	}

	total, err := svc.Count(ctx, "")
	require.NoError(t, err)
	require.EqualValues(t, 3, total)

	jobs, err := svc.List(ctx, job.ListParams{Limit: 10})
	require.NoError(t, err)
	require.Len(t, jobs, 3)
	// Newest-first.
	require.GreaterOrEqual(t, jobs[0].CreatedAt.Unix(), jobs[1].CreatedAt.Unix())
}

func TestCancelTransitionsQueuedJobAndIgnoresTerminal(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, job.CreateParams{
		Trigger: job.TriggerEphemeral,
		Name:    "x", Command: "x", Image: "x",
	})
	require.NoError(t, err)

	ok, err := svc.Cancel(ctx, created.ID)
	require.NoError(t, err)
	require.True(t, ok)

	got, err := svc.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, job.StatusCancelled, got.Status)
	require.NotNil(t, got.CompletedAt)

	// Cancelling again is a no-op (returns false, no error).
	ok, err = svc.Cancel(ctx, created.ID)
	require.NoError(t, err)
	require.False(t, ok)
}
