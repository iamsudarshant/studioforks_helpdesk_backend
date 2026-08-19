-- ---------------------------------------------------------------------------
-- 000016 Finish removing onboarding
--
-- 000014 dropped the onboarding wizard table, on the grounds that a client is
-- live from the moment it is created. The status enum was left behind, and with
-- it a column default of 'ONBOARDING' — so a client created through the API
-- still started in a state that no longer means anything, was hidden from the
-- lists that filter on ACTIVE, and could not be signed in to until somebody
-- noticed and activated it.
--
-- This removes the last of it: any client still sitting in that state becomes
-- live, and the default becomes ACTIVE so a newly created client works
-- immediately. ARCHIVED and SUSPENDED are untouched — those are real states.
-- ---------------------------------------------------------------------------

UPDATE tenants SET status = 'ACTIVE' WHERE status = 'ONBOARDING';

ALTER TABLE tenants
    MODIFY COLUMN status VARCHAR(20) NOT NULL DEFAULT 'ACTIVE';
