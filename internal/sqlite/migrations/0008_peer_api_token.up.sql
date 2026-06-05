-- Per-peer API token (only the hash is stored) for authenticating the daemon's
-- network-exposed API. Nullable: peers created before this, or without a token,
-- simply cannot use the remote API until re-paired.
ALTER TABLE peers ADD COLUMN api_token_hash TEXT;
