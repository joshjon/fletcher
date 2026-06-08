-- Published ports (Milestone 8): a port a durable session serves, brokered by
-- the daemon so the VM stays unroutable (the preview-proxy pattern, for an
-- arbitrary port). Phase 1 reaches a published port over the WireGuard tunnel
-- at tunnel_port; Phase 2 adds public exposure via the public flag + host.
--
-- tunnel_port is the host-side TCP port (on the tunnel IP) the daemon's port
-- broker listens on and forwards to guest_port inside the VM. It is assigned
-- once at publish time and reused across daemon restarts so a client's bookmark
-- holds. public/host are unused in Phase 1 (default off / NULL); Phase 2's
-- public listener routes a hostname to the matching guest_port.
CREATE TABLE published_ports (
    id          TEXT    NOT NULL PRIMARY KEY,
    session_id  TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    guest_port  INTEGER NOT NULL,
    name        TEXT    NOT NULL,
    tunnel_port INTEGER NOT NULL,
    public      INTEGER NOT NULL DEFAULT 0 CHECK (public IN (0, 1)),
    host        TEXT,
    created_at  INTEGER NOT NULL,
    UNIQUE (session_id, guest_port)
) STRICT;

CREATE INDEX idx_published_ports_session ON published_ports (session_id);

-- A public hostname maps to exactly one published port across all sessions.
CREATE UNIQUE INDEX idx_published_ports_host ON published_ports (host) WHERE host IS NOT NULL;
