-- name: CreateApproval :one
INSERT INTO pending_approvals (
    id, status, action, justification, requester, created_at, expires_at
) VALUES (
    ?, 'pending', ?, ?, ?, ?, ?
)
RETURNING *;

-- name: GetApproval :one
SELECT * FROM pending_approvals WHERE id = ?;

-- name: ListApprovals :many
SELECT * FROM pending_approvals
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: ListApprovalsByStatus :many
SELECT * FROM pending_approvals
WHERE status = ?
ORDER BY created_at DESC
LIMIT ? OFFSET ?;

-- name: CountApprovals :one
SELECT COUNT(*) FROM pending_approvals;

-- name: CountApprovalsByStatus :one
SELECT COUNT(*) FROM pending_approvals WHERE status = ?;

-- name: ApproveApproval :execrows
UPDATE pending_approvals
SET status = 'approved', decision_reason = ?, decided_at = ?
WHERE id = ? AND status = 'pending';

-- name: DenyApproval :execrows
UPDATE pending_approvals
SET status = 'denied', decision_reason = ?, decided_at = ?
WHERE id = ? AND status = 'pending';

-- name: ExpirePendingApprovals :execrows
UPDATE pending_approvals
SET status = 'expired', decided_at = ?
WHERE status = 'pending' AND expires_at < ?;
