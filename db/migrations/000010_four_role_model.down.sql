-- ---------------------------------------------------------------------------
-- Revert to the five-role model: KARMA_SUPER_ADMIN, KARMA_AGENT, PARTNER,
-- PARTNER_EXECUTIVE, EMPLOYEE, with SUPER_ADMIN and friends as aliases again.
--
-- AGENT is left in place rather than dropped. Deleting it would cascade to
-- user_roles and silently strip the role from every agent, so it is demoted to
-- an alias of KARMA_AGENT instead — the users keep working either way.
-- ---------------------------------------------------------------------------

UPDATE roles SET alias_of = NULL, is_deprecated = 0 WHERE tenant_id IS NULL;

UPDATE roles SET name = 'Karma Super Admin', portal = 'admin'
WHERE tenant_id IS NULL AND role_key = 'KARMA_SUPER_ADMIN';

UPDATE roles SET name = 'Karma Agent', portal = 'agents'
WHERE tenant_id IS NULL AND role_key = 'KARMA_AGENT';

UPDATE roles SET name = 'Partner Executive', portal = 'partner'
WHERE tenant_id IS NULL AND role_key = 'PARTNER_EXECUTIVE';

UPDATE roles SET alias_of = 'KARMA_SUPER_ADMIN', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('SUPER_ADMIN', 'HELPDESK_MASTER_ADMIN');

UPDATE roles SET alias_of = 'KARMA_AGENT', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('AGENT', 'HELPDESK_ADMIN', 'HELPDESK_EXECUTIVE');

UPDATE roles SET alias_of = 'PARTNER', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key = 'CLIENT_MASTER_ADMIN';

UPDATE roles SET alias_of = 'PARTNER_EXECUTIVE', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('ENTITY_ADMIN', 'DEPARTMENT_ADMIN');

UPDATE roles r
JOIN roles c ON c.tenant_id IS NULL AND c.role_key = r.alias_of
SET r.portal = c.portal
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

DELETE rp FROM role_permissions rp
JOIN roles r ON r.id = rp.role_id
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

INSERT INTO role_permissions (role_id, permission_key)
SELECT r.id, crp.permission_key
FROM roles r
JOIN roles c              ON c.tenant_id IS NULL AND c.role_key = r.alias_of
JOIN role_permissions crp ON crp.role_id = c.id
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

-- Move users back onto the five-role keys.
UPDATE user_roles ur
JOIN roles old ON old.id = ur.role_id AND old.tenant_id IS NULL AND old.alias_of IS NOT NULL
JOIN roles new ON new.tenant_id IS NULL AND new.role_key = old.alias_of
SET ur.role_id = new.id
WHERE NOT EXISTS (
    SELECT 1 FROM (SELECT * FROM user_roles) existing
    WHERE existing.user_id = ur.user_id AND existing.role_id = new.id);

UPDATE users SET email = REPLACE(email, '@complydesk.local', '@karma.local')
WHERE email IN ('superadmin@complydesk.local', 'agent.arjun@complydesk.local', 'agent.priya@complydesk.local');

UPDATE users SET employee_code = REPLACE(employee_code, 'CD-', 'KMG-')
WHERE employee_code IN ('CD-SA-001', 'CD-AG-001', 'CD-AG-002');
