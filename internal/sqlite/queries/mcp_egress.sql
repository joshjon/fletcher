-- name: CreateEgressOperation :exec
INSERT INTO mcp_egress_operations (caller_id, operation_key, request_hash, state, updated_at)
VALUES (?, ?, ?, 'pending', ?)
ON CONFLICT (caller_id, operation_key) DO NOTHING;

-- name: GetEgressOperation :one
SELECT caller_id, operation_key, request_hash, state, approval_id, result, updated_at
FROM mcp_egress_operations WHERE caller_id = ? AND operation_key = ?;

-- name: UpdateEgressApproval :exec
UPDATE mcp_egress_operations SET approval_id = ?, updated_at = ?
WHERE caller_id = ? AND operation_key = ?;

-- name: ClaimEgressOperation :execrows
UPDATE mcp_egress_operations SET state = 'executing', updated_at = ?
WHERE caller_id = ? AND operation_key = ? AND state = 'pending';

-- name: UpdateEgressResult :exec
UPDATE mcp_egress_operations SET state = ?, result = ?, updated_at = ?
WHERE caller_id = ? AND operation_key = ?;

-- name: InterruptEgressOperations :exec
UPDATE mcp_egress_operations SET state = 'unknown', updated_at = ?
WHERE state = 'executing';
