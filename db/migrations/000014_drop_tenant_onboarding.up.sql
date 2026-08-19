-- ---------------------------------------------------------------------------
-- 000014 Remove the client-onboarding wizard
--
-- Clients are live from the moment they are created, so the wizard table is
-- no longer written or read. Dropping it removes the dead provisioning
-- state; tenants.onboarded_at is kept for reporting on legacy rows.
-- ---------------------------------------------------------------------------

DROP TABLE IF EXISTS tenant_onboarding;
