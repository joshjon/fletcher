package api

import (
	"context"
	"time"

	"connectrpc.com/connect"

	"github.com/joshjon/fletcher/internal/approval"
	fletcherv1 "github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1"
	"github.com/joshjon/fletcher/internal/gen/proto/fletcher/v1/fletcherv1connect"
)

// ApprovalsBackend is the consumer-defined interface the ApprovalsService
// needs from the domain layer.
type ApprovalsBackend interface {
	Create(ctx context.Context, p approval.CreateParams) (approval.Approval, error)
	Get(ctx context.Context, id string) (approval.Approval, error)
	List(ctx context.Context, p approval.ListParams) ([]approval.Approval, error)
	Approve(ctx context.Context, id, reason string) (bool, error)
	Deny(ctx context.Context, id, reason string) (bool, error)
}

// ApprovalsService implements fletcherv1connect.ApprovalServiceHandler.
type ApprovalsService struct {
	fletcherv1connect.UnimplementedApprovalServiceHandler
	backend ApprovalsBackend
}

// NewApprovalsService wires an ApprovalsService to a backend.
func NewApprovalsService(backend ApprovalsBackend) *ApprovalsService {
	return &ApprovalsService{backend: backend}
}

// CreateApproval inserts a new pending approval.
func (s *ApprovalsService) CreateApproval(ctx context.Context, req *connect.Request[fletcherv1.CreateApprovalRequest]) (*connect.Response[fletcherv1.CreateApprovalResponse], error) {
	m := req.Msg
	got, err := s.backend.Create(ctx, approval.CreateParams{
		Action:        m.GetAction(),
		Justification: m.GetJustification(),
		Requester:     m.GetRequester(),
		TTL:           time.Duration(m.GetTtlSeconds()) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.CreateApprovalResponse{Approval: approvalToProto(got)}), nil
}

// GetApproval fetches a single approval. Lazily expires overdue rows.
func (s *ApprovalsService) GetApproval(ctx context.Context, req *connect.Request[fletcherv1.GetApprovalRequest]) (*connect.Response[fletcherv1.GetApprovalResponse], error) {
	got, err := s.backend.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.GetApprovalResponse{Approval: approvalToProto(got)}), nil
}

// ListApprovals returns approvals newest-first.
func (s *ApprovalsService) ListApprovals(ctx context.Context, req *connect.Request[fletcherv1.ListApprovalsRequest]) (*connect.Response[fletcherv1.ListApprovalsResponse], error) {
	m := req.Msg
	list, err := s.backend.List(ctx, approval.ListParams{
		Limit:        m.GetLimit(),
		Offset:       m.GetOffset(),
		StatusFilter: approvalStatusFromProto(m.GetStatusFilter()),
	})
	if err != nil {
		return nil, err
	}
	protos := make([]*fletcherv1.Approval, len(list))
	for i, a := range list {
		protos[i] = approvalToProto(a)
	}
	return connect.NewResponse(&fletcherv1.ListApprovalsResponse{Approvals: protos}), nil
}

// ApproveApproval transitions a pending row to approved.
func (s *ApprovalsService) ApproveApproval(ctx context.Context, req *connect.Request[fletcherv1.ApproveApprovalRequest]) (*connect.Response[fletcherv1.ApproveApprovalResponse], error) {
	decided, err := s.backend.Approve(ctx, req.Msg.GetId(), req.Msg.GetReason())
	if err != nil {
		return nil, err
	}
	a, err := s.backend.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.ApproveApprovalResponse{
		Decided:  decided,
		Approval: approvalToProto(a),
	}), nil
}

// DenyApproval transitions a pending row to denied.
func (s *ApprovalsService) DenyApproval(ctx context.Context, req *connect.Request[fletcherv1.DenyApprovalRequest]) (*connect.Response[fletcherv1.DenyApprovalResponse], error) {
	decided, err := s.backend.Deny(ctx, req.Msg.GetId(), req.Msg.GetReason())
	if err != nil {
		return nil, err
	}
	a, err := s.backend.Get(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&fletcherv1.DenyApprovalResponse{
		Decided:  decided,
		Approval: approvalToProto(a),
	}), nil
}

// --- proto ⇄ domain mapping ---

func approvalStatusFromProto(p fletcherv1.ApprovalStatus) approval.Status {
	switch p {
	case fletcherv1.ApprovalStatus_APPROVAL_STATUS_PENDING:
		return approval.StatusPending
	case fletcherv1.ApprovalStatus_APPROVAL_STATUS_APPROVED:
		return approval.StatusApproved
	case fletcherv1.ApprovalStatus_APPROVAL_STATUS_DENIED:
		return approval.StatusDenied
	case fletcherv1.ApprovalStatus_APPROVAL_STATUS_EXPIRED:
		return approval.StatusExpired
	}
	return ""
}

func approvalStatusToProto(s approval.Status) fletcherv1.ApprovalStatus {
	switch s {
	case approval.StatusPending:
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_PENDING
	case approval.StatusApproved:
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_APPROVED
	case approval.StatusDenied:
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_DENIED
	case approval.StatusExpired:
		return fletcherv1.ApprovalStatus_APPROVAL_STATUS_EXPIRED
	}
	return fletcherv1.ApprovalStatus_APPROVAL_STATUS_UNSPECIFIED
}

func approvalToProto(a approval.Approval) *fletcherv1.Approval {
	out := &fletcherv1.Approval{
		Id:             a.ID,
		Status:         approvalStatusToProto(a.Status),
		Action:         a.Action,
		Justification:  a.Justification,
		Requester:      a.Requester,
		DecisionReason: a.DecisionReason,
		CreatedAt:      a.CreatedAt.Unix(),
		ExpiresAt:      a.ExpiresAt.Unix(),
	}
	if a.DecidedAt != nil {
		t := a.DecidedAt.Unix()
		out.DecidedAt = &t
	}
	return out
}
