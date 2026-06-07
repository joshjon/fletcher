-- Per-fork egress policy (DESIGN.md §5): "none" | "allowlist" | "open". Gates
-- the daemon forward-proxy that brokers a fork's outbound network. Default
-- "allowlist" so existing forks keep the curated default; the create path
-- overrides it from the default_egress_policy setting (or an explicit --egress).
ALTER TABLE jobs ADD COLUMN egress_policy TEXT NOT NULL DEFAULT 'allowlist';
ALTER TABLE sessions ADD COLUMN egress_policy TEXT NOT NULL DEFAULT 'allowlist';
