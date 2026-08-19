-- ---------------------------------------------------------------------------
-- Reverse of 000016.
--
-- The column default goes back to 'ONBOARDING'. The rows are not moved back:
-- which clients were in that state before is not recorded anywhere, and
-- guessing would suspend live workspaces.
-- ---------------------------------------------------------------------------

ALTER TABLE tenants
    MODIFY COLUMN status VARCHAR(20) NOT NULL DEFAULT 'ONBOARDING';
