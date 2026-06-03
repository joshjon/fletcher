package approval_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/approval"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

func newService(t *testing.T) *approval.Service {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fletcher.db")
	db, err := sqlite.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	return approval.NewService(sqliteq.New(db), approval.ServiceOptions{
		PollInterval: 25 * time.Millisecond,
	})
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	got, err := s.Create(ctx, approval.CreateParams{
		Action:        "ssh into prod.example.com",
		Justification: "incident response",
		Requester:     "agent_xyz",
	})
	require.NoError(t, err)
	require.Equal(t, approval.StatusPending, got.Status)
	require.NotEmpty(t, got.ID)

	fetched, err := s.Get(ctx, got.ID)
	require.NoError(t, err)
	require.Equal(t, got.ID, fetched.ID)
	require.Equal(t, approval.StatusPending, fetched.Status)
}

func TestApproveTransitionsPendingAndRefusesTerminal(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	a, err := s.Create(ctx, approval.CreateParams{
		Action: "x", Justification: "y", Requester: "z",
	})
	require.NoError(t, err)

	ok, err := s.Approve(ctx, a.ID, "looks good")
	require.NoError(t, err)
	require.True(t, ok)

	fetched, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, approval.StatusApproved, fetched.Status)
	require.NotNil(t, fetched.DecidedAt)
	require.Equal(t, "looks good", fetched.DecisionReason)

	// Re-approving the same row should be a no-op (not an error).
	ok, err = s.Approve(ctx, a.ID, "again")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestDenyMutuallyExclusiveWithApprove(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	a, err := s.Create(ctx, approval.CreateParams{
		Action: "x", Justification: "y", Requester: "z",
	})
	require.NoError(t, err)

	ok, err := s.Deny(ctx, a.ID, "no thanks")
	require.NoError(t, err)
	require.True(t, ok)

	// Trying to approve a denied row does nothing.
	ok, err = s.Approve(ctx, a.ID, "wait actually yes")
	require.NoError(t, err)
	require.False(t, ok)

	fetched, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, approval.StatusDenied, fetched.Status)
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	s := newService(t)
	_, err := s.Get(context.Background(), "appr_missing")
	require.ErrorIs(t, err, approval.ErrNotFound)
}

func TestWaitReturnsImmediatelyOnDecidedRow(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	a, err := s.Create(ctx, approval.CreateParams{
		Action: "x", Justification: "y", Requester: "z",
	})
	require.NoError(t, err)
	_, err = s.Approve(ctx, a.ID, "")
	require.NoError(t, err)

	done := make(chan approval.Approval, 1)
	go func() {
		r, _ := s.Wait(ctx, a.ID)
		done <- r
	}()
	select {
	case r := <-done:
		require.Equal(t, approval.StatusApproved, r.Status)
	case <-time.After(time.Second):
		t.Fatal("Wait did not return on already-decided row")
	}
}

func TestWaitWakesOnDecision(t *testing.T) {
	s := newService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)

	a, err := s.Create(ctx, approval.CreateParams{
		Action: "x", Justification: "y", Requester: "z",
	})
	require.NoError(t, err)

	done := make(chan approval.Approval, 1)
	go func() {
		r, err := s.Wait(ctx, a.ID)
		require.NoError(t, err)
		done <- r
	}()

	// Decide after a short delay; Wait should wake up promptly.
	time.Sleep(50 * time.Millisecond)
	_, err = s.Approve(ctx, a.ID, "ok")
	require.NoError(t, err)

	select {
	case r := <-done:
		require.Equal(t, approval.StatusApproved, r.Status)
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not wake on decision")
	}
}

func TestWaitExpiresWhenTTLPasses(t *testing.T) {
	s := newService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)

	a, err := s.Create(ctx, approval.CreateParams{
		Action: "x", Justification: "y", Requester: "z",
		TTL: 100 * time.Millisecond,
	})
	require.NoError(t, err)

	r, err := s.Wait(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, approval.StatusExpired, r.Status)
}

func TestSweepExpiredMarksOverdueRows(t *testing.T) {
	s := newService(t)
	ctx := context.Background()

	// Schema stores expires_at as INTEGER seconds, so use a TTL of 1s
	// and sleep well past the next whole-second tick to guarantee
	// expires_at < now strictly.
	a, err := s.Create(ctx, approval.CreateParams{
		Action: "x", Justification: "y", Requester: "z",
		TTL: 1 * time.Second,
	})
	require.NoError(t, err)

	time.Sleep(2100 * time.Millisecond)
	n, err := s.SweepExpired(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	fetched, err := s.Get(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, approval.StatusExpired, fetched.Status)
}

func TestCreateValidatesRequiredFields(t *testing.T) {
	s := newService(t)
	cases := []struct {
		name string
		p    approval.CreateParams
	}{
		{"missing action", approval.CreateParams{Justification: "y", Requester: "z"}},
		{"missing justification", approval.CreateParams{Action: "x", Requester: "z"}},
		{"missing requester", approval.CreateParams{Action: "x", Justification: "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Create(context.Background(), tc.p)
			require.Error(t, err)
		})
	}
}
