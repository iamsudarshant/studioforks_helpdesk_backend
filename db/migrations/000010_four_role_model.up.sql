-- ---------------------------------------------------------------------------
-- 000010 Consolidate to ComplyDesk's four roles
--
--   SUPER_ADMIN  every client, every setting
--   AGENT        ComplyDesk support staff; resolves tickets and administers
--                clients across the platform
--   PARTNER      client-side admin/HR
--   EMPLOYEE     end user
--
-- Two things change beyond naming:
--
--   1. KARMA_SUPER_ADMIN and KARMA_AGENT were canonical and SUPER_ADMIN was a
--      deprecated alias of the former. That relationship inverts here, so the
--      flags have to be cleared before they are re-set or a row would briefly
--      point at itself.
--
--   2. PARTNER_EXECUTIVE folds into PARTNER. The distinction was never carried
--      by the role — it was the entity/site/department scope attached to the
--      user, in user_scopes, which this migration does not touch. A previously
--      scoped executive therefore keeps seeing exactly what they saw.
--
-- Users ARE moved onto the canonical keys at the end, because leaving them on
-- deprecated ones would mean the model is only half-adopted. The move is safe:
-- permissions are mirrored first, so nobody gains or loses anything. The aliases
-- remain for external integrations and for any tenant-scoped custom role that
-- still references them.
-- ---------------------------------------------------------------------------

-- Break every existing alias link first, so the re-linking below cannot chain
-- (A -> B -> C) or cycle when a key changes side.
UPDATE roles SET alias_of = NULL, is_deprecated = 0 WHERE tenant_id IS NULL;

-- --- the four canonical roles ----------------------------------------------

-- SUPER_ADMIN and EMPLOYEE already exist under those names. AGENT is new;
-- PARTNER exists. Insert only what is missing, so a re-run is a no-op.
INSERT INTO roles (public_id, tenant_id, role_key, name, description, portal, is_system, is_deprecated)
SELECT CONCAT('01', UPPER(SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24))),
       NULL, 'AGENT', 'Agent',
       'ComplyDesk support staff. Resolves tickets for every client, and creates and administers clients, their organisation structure and their users.',
       'agents', 1, 0
WHERE NOT EXISTS (SELECT 1 FROM roles WHERE tenant_id IS NULL AND role_key = 'AGENT');

UPDATE roles SET name = 'Super Admin', portal = 'admin', is_system = 1
WHERE tenant_id IS NULL AND role_key = 'SUPER_ADMIN';

UPDATE roles SET name = 'Partner', portal = 'partner', is_system = 1
WHERE tenant_id IS NULL AND role_key = 'PARTNER';

UPDATE roles SET name = 'Employee', portal = 'user', is_system = 1
WHERE tenant_id IS NULL AND role_key = 'EMPLOYEE';

-- --- the deprecated keys ----------------------------------------------------

UPDATE roles SET alias_of = 'SUPER_ADMIN', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('KARMA_SUPER_ADMIN', 'HELPDESK_MASTER_ADMIN');

UPDATE roles SET alias_of = 'AGENT', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('KARMA_AGENT', 'HELPDESK_ADMIN', 'HELPDESK_EXECUTIVE');

UPDATE roles SET alias_of = 'PARTNER', is_deprecated = 1
WHERE tenant_id IS NULL
  AND role_key IN ('CLIENT_MASTER_ADMIN', 'PARTNER_EXECUTIVE', 'ENTITY_ADMIN', 'DEPARTMENT_ADMIN');

-- A deprecated role must never sit on a portal its replacement does not serve,
-- or a user on the old key would be refused at login by the portal check.
UPDATE roles r
JOIN roles c ON c.tenant_id IS NULL AND c.role_key = r.alias_of
SET r.portal = c.portal
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

-- Mirror each canonical role's permissions onto its aliases, so a user still
-- holding an old key behaves identically to one on the replacement. `seed
-- platform` does this too; doing it here means a migrated database is correct
-- before the seeder is ever run.
DELETE rp FROM role_permissions rp
JOIN roles r ON r.id = rp.role_id
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

INSERT INTO role_permissions (role_id, permission_key)
SELECT r.id, crp.permission_key
FROM roles r
JOIN roles c        ON c.tenant_id IS NULL AND c.role_key = r.alias_of
JOIN role_permissions crp ON crp.role_id = c.id
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

-- --- move users onto the canonical roles ------------------------------------

-- Permissions were mirrored above, so this changes nobody's access. It runs
-- last for exactly that reason.
--
-- The guard against an existing row matters: a user could already hold both the
-- deprecated key and its replacement (an administrator granted both), and
-- uq_user_roles would reject the duplicate.
UPDATE user_roles ur
JOIN roles old ON old.id = ur.role_id AND old.tenant_id IS NULL AND old.alias_of IS NOT NULL
JOIN roles new ON new.tenant_id IS NULL AND new.role_key = old.alias_of
SET ur.role_id = new.id
WHERE NOT EXISTS (
    SELECT 1 FROM (SELECT * FROM user_roles) existing
    WHERE existing.user_id = ur.user_id AND existing.role_id = new.id);

-- Anyone left holding both keys keeps only the canonical one.
DELETE ur FROM user_roles ur
JOIN roles old ON old.id = ur.role_id AND old.tenant_id IS NULL AND old.alias_of IS NOT NULL
WHERE EXISTS (
    SELECT 1 FROM (SELECT * FROM user_roles) existing
    JOIN roles new ON new.id = existing.role_id
    WHERE existing.user_id = ur.user_id AND new.tenant_id IS NULL AND new.role_key = old.alias_of);

-- --- seeded staff addresses -------------------------------------------------

-- The demo staff were seeded under @karma.local before the product name settled.
-- Renaming them keeps a seeded database matching the documented credentials.
-- Scoped to the exact seeded addresses so no real account is touched.
UPDATE users SET email = REPLACE(email, '@karma.local', '@complydesk.local')
WHERE email IN ('superadmin@karma.local', 'agent.arjun@karma.local', 'agent.priya@karma.local');

UPDATE users SET employee_code = REPLACE(employee_code, 'KMG-', 'CD-')
WHERE employee_code IN ('KMG-SA-001', 'KMG-AG-001', 'KMG-AG-002');
