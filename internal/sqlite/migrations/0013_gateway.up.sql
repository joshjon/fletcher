-- Per-fork model-gateway toggle (DESIGN.md §6): "on" injects the daemon's
-- gateway env (ANTHROPIC_/OPENAI_ base-URL + placeholder key) so agents route
-- model calls through the daemon; "off" omits it so an agent uses its own auth
-- (e.g. a subscription OAuth login) and reaches the provider via egress. Default
-- "on" keeps the existing turnkey behaviour; the create path overrides it from
-- the default_gateway setting (or an explicit --gateway).
ALTER TABLE jobs ADD COLUMN gateway TEXT NOT NULL DEFAULT 'on';
ALTER TABLE sessions ADD COLUMN gateway TEXT NOT NULL DEFAULT 'on';
