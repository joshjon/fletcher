// Package approval implements the daemon's privileged-operation approval
// queue. Agents (via MCP tools) request human consent; users grant or
// deny through the CLI / future native clients. Per DESIGN.md §5,
// approvals are SQLite rows so they survive a daemon restart unchanged.
//
// APNs push is deferred (phase 7+); for now consumers poll Wait to
// observe decisions.
package approval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.jetify.com/typeid"

	"github.com/joshjon/fletcher/internal/errs"
	sqliteq "github.com/joshjon/fletcher/internal/sqlite/gen"
)

// Status is the lifecycle state of an approval request.
type Status string

// Status values mirror the schema CHECK clause.
const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusDenied   Status = "denied"
	StatusExpired  Status = "expired"
)

// idPrefix is the typeid prefix for approval IDs.
const idPrefix = "appr"

// ErrNotFound is returned when an approval ID does not exist.
var ErrNotFound = errs.New(errs.CategoryNotFound, "approval not found")

// Approval is the domain shape of a pending-approval row.
type Approval struct {
	ID             string
	Status         Status
	Action         string
	Justification  string
	Requester      string
	DecisionReason string
	CreatedAt      time.Time
	DecidedAt      *time.Time
	ExpiresAt      time.Time
}

// IsTerminal reports whether a is in a final (decided or expired) state.
func (a Approval) IsTerminal() bool { return a.Status != StatusPending }

// CreateParams are the inputs to a new approval request.
type CreateParams struct {
	Action        string
	Justification string
	Requester     string
	TTL           time.Duration // optional; default 5 minutes when zero
}

// DefaultTTL is the lifetime of a pending approval if the caller does
// not specify one.
const DefaultTTL = 5 * time.Minute

// Service is the high-level approvals API.
type Service struct {
	q sqliteq.Querier
	// wakeCh signals waiters that the DB has changed. Capacity 1 so
	// repeated decisions in flight coalesce.
	wakeCh chan struct{}

	// pollInterval bounds wait latency in case the wake signal is missed
	// (we only fire wakeCh from in-process Approve/Deny — external writes
	// would otherwise be invisible). Tests can override.
	pollInterval time.Duration
}

// ServiceOptions configures a Service.
type ServiceOptions struct {
	// PollInterval bounds how long Wait can lag a decision. Default 500ms.
	PollInterval time.Duration
}

// NewService wires a Service to a sqlc-generated querier.
func NewService(q sqliteq.Querier, opts ServiceOptions) *Service {
	if opts.PollInterval == 0 {
		opts.PollInterval = 500 * time.Millisecond
	}
	return &Service{
		q:            q,
		wakeCh:       make(chan struct{}, 1),
		pollInterval: opts.PollInterval,
	}
}

// Create inserts a new pending approval.
func (s *Service) Create(ctx context.Context, p CreateParams) (Approval, error) {
	if err := p.validate(); err != nil {
		return Approval{}, err
	}
	ttl := p.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	id, err := typeid.WithPrefix(idPrefix)
	if err != nil {
		return Approval{}, fmt.Errorf("generate id: %w", err)
	}
	now := time.Now()
	row, err := s.q.CreateApproval(ctx, sqliteq.CreateApprovalParams{
		ID:            id.String(),
		Action:        p.Action,
		Justification: p.Justification,
		Requester:     p.Requester,
		CreatedAt:     now.Unix(),
		ExpiresAt:     now.Add(ttl).Unix(),
	})
	if err != nil {
		return Approval{}, fmt.Errorf("create approval: %w", err)
	}
	return approvalFromRow(row), nil
}

// Get returns a single approval, expiring it lazily on read if its
// expires_at has passed.
func (s *Service) Get(ctx context.Context, id string) (Approval, error) {
	row, err := s.q.GetApproval(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNotFound
	}
	if err != nil {
		return Approval{}, fmt.Errorf("get approval: %w", err)
	}
	a := approvalFromRow(row)
	if a.Status == StatusPending && time.Now().After(a.ExpiresAt) {
		// Best-effort lazy expiry; ignore error here, the row will sweep eventually.
		_, _ = s.q.ExpirePendingApprovals(ctx, sqliteq.ExpirePendingApprovalsParams{
			DecidedAt: ptrInt64(time.Now().Unix()),
			ExpiresAt: time.Now().Unix(),
		})
		a.Status = StatusExpired
		now := time.Now()
		a.DecidedAt = &now
	}
	return a, nil
}

// ListParams paginates the approvals list.
type ListParams struct {
	Limit        int32
	Offset       int32
	StatusFilter Status
}

// List returns approvals newest-first, optionally filtered by status.
func (s *Service) List(ctx context.Context, p ListParams) ([]Approval, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.StatusFilter != "" {
		rows, err := s.q.ListApprovalsByStatus(ctx, sqliteq.ListApprovalsByStatusParams{
			Status: string(p.StatusFilter),
			Limit:  int64(p.Limit),
			Offset: int64(p.Offset),
		})
		if err != nil {
			return nil, fmt.Errorf("list approvals by status: %w", err)
		}
		return mapApprovals(rows), nil
	}
	rows, err := s.q.ListApprovals(ctx, sqliteq.ListApprovalsParams{
		Limit:  int64(p.Limit),
		Offset: int64(p.Offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	return mapApprovals(rows), nil
}

// Approve transitions a pending approval to approved. reason is optional.
// Returns false (no error) if the approval was already terminal.
func (s *Service) Approve(ctx context.Context, id, reason string) (bool, error) {
	return s.decide(ctx, id, reason, true)
}

// Deny transitions a pending approval to denied. reason is optional.
// Returns false (no error) if the approval was already terminal.
func (s *Service) Deny(ctx context.Context, id, reason string) (bool, error) {
	return s.decide(ctx, id, reason, false)
}

func (s *Service) decide(ctx context.Context, id, reason string, approve bool) (bool, error) {
	now := time.Now().Unix()
	var (
		n   int64
		err error
	)
	if approve {
		n, err = s.q.ApproveApproval(ctx, sqliteq.ApproveApprovalParams{
			DecisionReason: nilIfEmpty(reason),
			DecidedAt:      ptrInt64(now),
			ID:             id,
		})
	} else {
		n, err = s.q.DenyApproval(ctx, sqliteq.DenyApprovalParams{
			DecisionReason: nilIfEmpty(reason),
			DecidedAt:      ptrInt64(now),
			ID:             id,
		})
	}
	if err != nil {
		return false, fmt.Errorf("decide approval: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	s.notify()
	return true, nil
}

// Wait blocks until the approval reaches a terminal state, ctx is
// cancelled, or its expires_at has passed. It returns the final Approval
// (possibly with Status=Expired). ErrNotFound is returned if the approval
// doesn't exist.
func (s *Service) Wait(ctx context.Context, id string) (Approval, error) {
	for {
		a, err := s.Get(ctx, id)
		if err != nil {
			return Approval{}, err
		}
		if a.IsTerminal() {
			return a, nil
		}
		// Sleep until: wake signal, poll interval, ctx cancel, or expiry.
		remaining := time.Until(a.ExpiresAt)
		timer := time.NewTimer(s.pollInterval)
		expiry := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			expiry.Stop()
			return Approval{}, ctx.Err()
		case <-s.wakeCh:
			timer.Stop()
			expiry.Stop()
		case <-timer.C:
			expiry.Stop()
		case <-expiry.C:
			timer.Stop()
		}
	}
}

// SweepExpired marks all pending approvals whose expires_at has passed
// as expired. Intended to be called periodically by a background loop;
// callers may also rely on lazy expiry via Get.
func (s *Service) SweepExpired(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	n, err := s.q.ExpirePendingApprovals(ctx, sqliteq.ExpirePendingApprovalsParams{
		DecidedAt: ptrInt64(now),
		ExpiresAt: now,
	})
	if err != nil {
		return 0, fmt.Errorf("expire pending approvals: %w", err)
	}
	if n > 0 {
		s.notify()
	}
	return n, nil
}

func (s *Service) notify() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (p CreateParams) validate() error {
	if strings.TrimSpace(p.Action) == "" {
		return errs.New(errs.CategoryInvalidArgument, "action is required")
	}
	if strings.TrimSpace(p.Justification) == "" {
		return errs.New(errs.CategoryInvalidArgument, "justification is required")
	}
	if strings.TrimSpace(p.Requester) == "" {
		return errs.New(errs.CategoryInvalidArgument, "requester is required")
	}
	return nil
}

func mapApprovals(rows []sqliteq.PendingApproval) []Approval {
	out := make([]Approval, len(rows))
	for i, r := range rows {
		out[i] = approvalFromRow(r)
	}
	return out
}

func approvalFromRow(r sqliteq.PendingApproval) Approval {
	a := Approval{
		ID:            r.ID,
		Status:        Status(r.Status),
		Action:        r.Action,
		Justification: r.Justification,
		Requester:     r.Requester,
		CreatedAt:     time.Unix(r.CreatedAt, 0).UTC(),
		ExpiresAt:     time.Unix(r.ExpiresAt, 0).UTC(),
	}
	if r.DecisionReason != nil {
		a.DecisionReason = *r.DecisionReason
	}
	if r.DecidedAt != nil {
		t := time.Unix(*r.DecidedAt, 0).UTC()
		a.DecidedAt = &t
	}
	return a
}

func ptrInt64(v int64) *int64 { return &v }

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
