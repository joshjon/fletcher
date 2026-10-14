# MCP egress safety

MCP requests from real forks carry host-authenticated identity and the fork's
boot-time egress policy. The host relay replaces guest-supplied identity headers
and signs attribution with a per-process secret that never enters the guest.
Firecracker uses its host-side vsock relay; runc mounts only its per-job relay
socket. Direct calls to the shared MCP endpoint without this identity are rejected.
The mock runtime is not isolated and does not receive trusted MCP access.

`http_get` and `http_request` apply the same curated allowlist as the forward
proxy. `none` denies these tools' internet access; `open` permits public hosts.
Every redirect is checked. The existing dial-time LAN/metadata guard remains in
force. The optional model gateway is a separate capability: enabling it can send
model requests even when general internet egress is disabled (DESIGN.md section 6).

HTTP GET/HEAD are reads. POST/PUT/PATCH/DELETE require a justification and a stable
`idempotency_key` of 16-80 letters, digits, underscores or hyphens. Fletcher creates
an approval before sending and stores a per-fork operation record in SQLite.
Retries of a completed request return its bounded saved result without sending
again. A key cannot be reused for a different method, URL, headers or body.
Pending approvals can be resumed with the same key after disconnect or restart.
Request credentials, headers and bodies are not stored: only a request digest,
operation state, approval reference and bounded response are retained. Approval
cards omit URL credentials and query strings.

This is at-most-once sending, not a claim of exactly-once delivery. If a crash or
transport failure makes the outcome ambiguous, the operation becomes `unknown`
and is not sent again automatically. Inspect the destination before deliberately
starting a new operation with a new key. Write redirects and automatic body
replay are disabled. This gate covers MCP HTTP writes, not arbitrary application
traffic inside an explicitly internet-enabled fork.

Automated checks cover forged identity, host-header replacement, policy denial,
redirect policy, approval denial/reuse, concurrent retries, persisted results and
interrupted-send handling. Hardware verification still needs a real session and
job in each isolated runtime: test `none`, an allowed host, an unlisted host,
redirects, approval and reconnect with the same operation key.
