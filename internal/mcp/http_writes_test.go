package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/joshjon/fletcher/internal/approval"
	"github.com/joshjon/fletcher/internal/forkauth"
	"github.com/joshjon/fletcher/internal/sqlite"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

type decidingApprovals struct {
	*approval.Service
	deny bool
}

func (a decidingApprovals) Create(ctx context.Context, p approval.CreateParams) (approval.Approval, error) {
	created, err := a.Service.Create(ctx, p)
	if err != nil {
		return created, err
	}
	if a.deny {
		_, err = a.Deny(ctx, created.ID, "test denial")
	} else {
		_, err = a.Approve(ctx, created.ID, "test consent")
	}
	return created, err
}

func writeStore(t *testing.T, client *http.Client, deny bool) (*HTTPWrites, *sqliteq.Queries) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, sqlite.Migrate(db))
	q := sqliteq.New(db)
	s, err := NewHTTPWrites(t.Context(), q, HTTPWritesOptions{Client: client, Approvals: decidingApprovals{Service: approval.NewService(q, approval.ServiceOptions{}), deny: deny}})
	require.NoError(t, err)
	return s, q
}

func writeRequest(t *testing.T, url, body string) (context.Context, *http.Request) {
	t.Helper()
	ctx := forkauth.WithCaller(t.Context(), forkauth.Caller{ID: "session_example", Policy: "open", Session: true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	return ctx, req
}

func TestHTTPWriteReplaysPersistedResultAcrossRestart(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("missing upstream idempotency key")
		}
		_, _ = io.WriteString(w, "created")
	}))
	defer srv.Close()
	s, q := writeStore(t, srv.Client(), false)
	ctx, req := writeRequest(t, srv.URL+"?token=placeholder-secret", "body")
	result, err := s.Execute(ctx, req, "operation_example", "create an example")
	require.NoError(t, err)
	require.Contains(t, result, "created")
	restarted, err := NewHTTPWrites(t.Context(), q, HTTPWritesOptions{Client: srv.Client(), Approvals: decidingApprovals{Service: approval.NewService(q, approval.ServiceOptions{})}})
	require.NoError(t, err)
	ctx, req = writeRequest(t, srv.URL+"?token=placeholder-secret", "body")
	again, err := restarted.Execute(ctx, req, "operation_example", "retry")
	require.NoError(t, err)
	require.Equal(t, result, again)
	require.EqualValues(t, 1, calls.Load())
	approvals, err := approval.NewService(q, approval.ServiceOptions{}).List(ctx, approval.ListParams{})
	require.NoError(t, err)
	require.Len(t, approvals, 1)
	require.NotContains(t, approvals[0].Action, "placeholder-secret")
	ctx, req = writeRequest(t, srv.URL, "different body")
	_, err = restarted.Execute(ctx, req, "operation_example", "retry")
	require.ErrorContains(t, err, "different request")
	require.EqualValues(t, 1, calls.Load())
}

func TestHTTPWriteDenialNeverSends(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("must not send") })}
	s, _ := writeStore(t, client, true)
	ctx, req := writeRequest(t, "https://example.com", "body")
	_, err := s.Execute(ctx, req, "operation_example", "example write")
	require.ErrorContains(t, err, "not approved")
	require.Zero(t, calls.Load())
}

func TestHTTPWriteUnknownOutcomeNeverRetries(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, io.ErrUnexpectedEOF })}
	s, q := writeStore(t, client, false)
	for range 2 {
		ctx, req := writeRequest(t, "https://example.com", "body")
		_, err := s.Execute(ctx, req, "operation_example", "example write")
		require.ErrorContains(t, err, "outcome is unknown")
	}
	require.EqualValues(t, 1, calls.Load())
	// Simulate death after the durable claim but before result recording.
	require.NoError(t, q.CreateEgressOperation(t.Context(), sqliteq.CreateEgressOperationParams{CallerID: "session_example", OperationKey: "interrupted_example", RequestHash: "hash"}))
	_, err := q.ClaimEgressOperation(t.Context(), sqliteq.ClaimEgressOperationParams{CallerID: "session_example", OperationKey: "interrupted_example"})
	require.NoError(t, err)
	_, err = NewHTTPWrites(t.Context(), q, HTTPWritesOptions{Client: client, Approvals: decidingApprovals{Service: approval.NewService(q, approval.ServiceOptions{})}})
	require.NoError(t, err)
	row, err := q.GetEgressOperation(t.Context(), sqliteq.GetEgressOperationParams{CallerID: "session_example", OperationKey: "interrupted_example"})
	require.NoError(t, err)
	require.Equal(t, "unknown", row.State)
}

type interruptedWait struct{ *approval.Service }

func (a interruptedWait) Wait(context.Context, string) (approval.Approval, error) {
	return approval.Approval{}, context.Canceled
}

func TestHTTPWriteResumesSamePendingApproval(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "created")
	}))
	defer srv.Close()
	s, q := writeStore(t, srv.Client(), false)
	approvals := approval.NewService(q, approval.ServiceOptions{})
	s.approvals = interruptedWait{approvals}
	ctx, req := writeRequest(t, srv.URL, "body")
	_, err := s.Execute(ctx, req, "operation_example", "example write")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, calls.Load())
	pending, err := approvals.List(t.Context(), approval.ListParams{})
	require.NoError(t, err)
	require.Len(t, pending, 1)
	_, err = approvals.Approve(t.Context(), pending[0].ID, "resume")
	require.NoError(t, err)
	restarted, err := NewHTTPWrites(t.Context(), q, HTTPWritesOptions{Client: srv.Client(), Approvals: approvals})
	require.NoError(t, err)
	ctx, req = writeRequest(t, srv.URL, "body")
	_, err = restarted.Execute(ctx, req, "operation_example", "retry")
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
	pending, err = approvals.List(t.Context(), approval.ListParams{})
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestConcurrentHTTPWriteSendsOnce(t *testing.T) {
	var calls atomic.Int32
	entered, proceed := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-proceed
		_, _ = io.WriteString(w, "created")
	}))
	defer srv.Close()
	s, _ := writeStore(t, srv.Client(), false)
	results := make(chan error, 2)
	ctx, first := writeRequest(t, srv.URL, "body")
	go func() { _, err := s.Execute(ctx, first, "operation_example", "example write"); results <- err }()
	<-entered
	ctx, second := writeRequest(t, srv.URL, "body")
	go func() { _, err := s.Execute(ctx, second, "operation_example", "retry"); results <- err }()
	close(proceed)
	for range 2 {
		require.NoError(t, <-results)
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestHTTPWriteDoesNotFollowRedirects(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/different", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	s, _ := writeStore(t, srv.Client(), false)
	ctx, req := writeRequest(t, srv.URL, "body")
	result, err := s.Execute(ctx, req, "operation_example", "example write")
	require.NoError(t, err)
	require.Contains(t, result, "status=307")
	require.EqualValues(t, 1, calls.Load())
}
