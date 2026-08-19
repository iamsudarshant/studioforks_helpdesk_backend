-- ---------------------------------------------------------------------------
-- Reverse of 000017.
--
-- Restoring the unique index will fail if any two accounts now share a PAN —
-- which is precisely the state this migration made legal. Those rows must be
-- reconciled by hand first; failing loudly is better than silently dropping
-- one of the two people involved.
-- ---------------------------------------------------------------------------

DROP INDEX ix_users_pan ON users;

ALTER TABLE users ADD UNIQUE KEY uq_users_pan (tenant_id, pan_number);
