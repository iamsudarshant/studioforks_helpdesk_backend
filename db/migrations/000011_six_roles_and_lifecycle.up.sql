-- ---------------------------------------------------------------------------
-- 000011 The six-role model, and the specified ticket lifecycle
--
-- Roles
--   SUPER_ADMIN         complete system access
--   HELPDESK_HEAD       runs the desk across every client
--   HELPDESK_EXECUTIVE  works tickets across every client
--   CLIENT_ADMIN        all entities allocated to them; reply is opt-in
--   CLIENT_EXECUTIVE    one entity/location; raises on behalf of employees
--   EMPLOYEE            end user
--
-- AGENT folds into HELPDESK_HEAD and PARTNER into CLIENT_ADMIN — in each case
-- the wider of the two available roles, so nobody loses access they had.
--
-- Lifecycle
--   NEW -> OPEN -> IN_PROGRESS <-> PENDING_EMPLOYEE / PENDING_HELPDESK
--                               -> RESOLVED -> CLOSED
--   plus ESCALATED, REOPENED, CANCELLED
--
-- NEW and OPEN are now distinct: NEW means unreviewed, OPEN means the helpdesk
-- has accepted it. The interval between them is first-response time, which is
-- what the SLA is judged on — so this split is what makes first-response
-- reporting possible at all.
--
-- Existing rows map as:
--   OPEN      -> NEW               (nobody had looked at these)
--   ASSIGNED  -> OPEN              (someone had)
--   PENDING_INFORMATION -> PENDING_EMPLOYEE
--   PENDING_DEPT        -> PENDING_HELPDESK
--
-- The OPEN -> NEW and ASSIGNED -> OPEN pair must run in that order, or the
-- first would rename the rows the second is looking for.
-- ---------------------------------------------------------------------------

-- --- roles ------------------------------------------------------------------

UPDATE roles SET alias_of = NULL, is_deprecated = 0 WHERE tenant_id IS NULL;

INSERT INTO roles (public_id, tenant_id, role_key, name, description, portal, is_system, is_deprecated)
SELECT CONCAT('01', UPPER(SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24))), NULL, k.role_key, k.name, k.description, k.portal, 1, 0
FROM (
    SELECT 'HELPDESK_HEAD' AS role_key, 'Helpdesk Head' AS name, 'agents' AS portal,
           'Runs the desk across every client: assigns, transfers, escalates, closes, and administers clients.' AS description
    UNION ALL SELECT 'HELPDESK_EXECUTIVE', 'Helpdesk Executive', 'agents',
           'Works tickets across every client: responds, requests information, uploads documents, updates status.'
    UNION ALL SELECT 'CLIENT_ADMIN', 'Client Admin', 'partner',
           'Sees every entity allocated to them: dashboards, search, SLA monitoring, reports. Reply is granted per user.'
    UNION ALL SELECT 'CLIENT_EXECUTIVE', 'Client Executive', 'partner',
           'Segmented to one entity or location. Raises tickets on behalf of its employees.'
) AS k
WHERE NOT EXISTS (SELECT 1 FROM roles r WHERE r.tenant_id IS NULL AND r.role_key = k.role_key);

UPDATE roles SET name = 'Super Admin', portal = 'admin', is_system = 1
WHERE tenant_id IS NULL AND role_key = 'SUPER_ADMIN';
UPDATE roles SET name = 'Employee', portal = 'user', is_system = 1
WHERE tenant_id IS NULL AND role_key = 'EMPLOYEE';

UPDATE roles SET alias_of = 'SUPER_ADMIN', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('KARMA_SUPER_ADMIN', 'HELPDESK_MASTER_ADMIN');

UPDATE roles SET alias_of = 'HELPDESK_HEAD', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('AGENT', 'KARMA_AGENT', 'HELPDESK_ADMIN');

UPDATE roles SET alias_of = 'CLIENT_ADMIN', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('PARTNER', 'CLIENT_MASTER_ADMIN');

UPDATE roles SET alias_of = 'CLIENT_EXECUTIVE', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('PARTNER_EXECUTIVE', 'ENTITY_ADMIN', 'DEPARTMENT_ADMIN');

-- A deprecated role must sit on the portal its replacement serves, or a user
-- still holding the old key is refused at login by the portal check.
UPDATE roles r
JOIN roles c ON c.tenant_id IS NULL AND c.role_key = r.alias_of
SET r.portal = c.portal
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

-- Mirror each canonical role's permissions onto its aliases, so a user still
-- holding an old key behaves identically to one on the replacement — and so the
-- repointing at the end of this file cannot change anyone's access.
DELETE rp FROM role_permissions rp
JOIN roles r ON r.id = rp.role_id
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

INSERT INTO role_permissions (role_id, permission_key)
SELECT r.id, crp.permission_key
FROM roles r
JOIN roles c              ON c.tenant_id IS NULL AND c.role_key = r.alias_of
JOIN role_permissions crp ON crp.role_id = c.id
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

-- --- lifecycle --------------------------------------------------------------

-- Order matters: ASSIGNED must become OPEN only after the old OPEN rows have
-- become NEW, or both would collapse into one state.
UPDATE tickets SET status = 'NEW'              WHERE status = 'OPEN';
UPDATE tickets SET status = 'OPEN'             WHERE status = 'ASSIGNED';
UPDATE tickets SET status = 'PENDING_EMPLOYEE' WHERE status IN ('PENDING_INFORMATION', 'PENDING_USER');
UPDATE tickets SET status = 'PENDING_HELPDESK' WHERE status = 'PENDING_DEPT';

-- The history is an audit record, so it is remapped rather than rewritten: the
-- same transition must read the same way whichever vocabulary it was recorded
-- under. `from_status` and `to_status` are handled separately because a single
-- row can carry the old name in one column and a new one in the other.
UPDATE ticket_status_history SET from_status = 'NEW'  WHERE from_status = 'OPEN';
UPDATE ticket_status_history SET to_status   = 'NEW'  WHERE to_status   = 'OPEN';
UPDATE ticket_status_history SET from_status = 'OPEN' WHERE from_status = 'ASSIGNED';
UPDATE ticket_status_history SET to_status   = 'OPEN' WHERE to_status   = 'ASSIGNED';
UPDATE ticket_status_history SET from_status = 'PENDING_EMPLOYEE' WHERE from_status IN ('PENDING_INFORMATION', 'PENDING_USER');
UPDATE ticket_status_history SET to_status   = 'PENDING_EMPLOYEE' WHERE to_status   IN ('PENDING_INFORMATION', 'PENDING_USER');
UPDATE ticket_status_history SET from_status = 'PENDING_HELPDESK' WHERE from_status = 'PENDING_DEPT';
UPDATE ticket_status_history SET to_status   = 'PENDING_HELPDESK' WHERE to_status   = 'PENDING_DEPT';

UPDATE category_workflows SET from_status = 'NEW'  WHERE from_status = 'OPEN';
UPDATE category_workflows SET to_status   = 'NEW'  WHERE to_status   = 'OPEN';
UPDATE category_workflows SET from_status = 'OPEN' WHERE from_status = 'ASSIGNED';
UPDATE category_workflows SET to_status   = 'OPEN' WHERE to_status   = 'ASSIGNED';
UPDATE category_workflows SET from_status = 'PENDING_EMPLOYEE' WHERE from_status IN ('PENDING_INFORMATION', 'PENDING_USER');
UPDATE category_workflows SET to_status   = 'PENDING_EMPLOYEE' WHERE to_status   IN ('PENDING_INFORMATION', 'PENDING_USER');
UPDATE category_workflows SET from_status = 'PENDING_HELPDESK' WHERE from_status = 'PENDING_DEPT';
UPDATE category_workflows SET to_status   = 'PENDING_HELPDESK' WHERE to_status   = 'PENDING_DEPT';

-- The remap can leave two rows describing the same transition (an OPEN->X and
-- an ASSIGNED->X that both became OPEN->X). Collapse them, keeping the lowest
-- id so the surviving row is the one referenced by anything else.
DELETE w FROM category_workflows w
JOIN category_workflows keep
  ON keep.tenant_id = w.tenant_id AND keep.category_id = w.category_id
 AND keep.from_status = w.from_status AND keep.to_status = w.to_status
 AND keep.id < w.id;

-- A ticket cannot move to a status it is already in.
DELETE FROM category_workflows WHERE from_status = to_status;

-- Escalation is a status now, not only a counter, so it appears on the board.
INSERT INTO category_workflows
    (tenant_id, category_id, from_status, to_status, label, requires_comment, requires_reason_code, is_active)
SELECT DISTINCT w.tenant_id, w.category_id, f.status, 'ESCALATED', 'Escalate', 1, 0, 1
FROM category_workflows w
CROSS JOIN (SELECT 'OPEN' AS status UNION ALL SELECT 'IN_PROGRESS' UNION ALL SELECT 'PENDING_HELPDESK') AS f
WHERE NOT EXISTS (
    SELECT 1 FROM category_workflows x
    WHERE x.tenant_id = w.tenant_id AND x.category_id = w.category_id
      AND x.from_status = f.status AND x.to_status = 'ESCALATED');

-- An escalated ticket has to be able to come back down, or it is a dead end.
INSERT INTO category_workflows
    (tenant_id, category_id, from_status, to_status, label, requires_comment, requires_reason_code, is_active)
SELECT DISTINCT w.tenant_id, w.category_id, 'ESCALATED', t.status, t.label, 0, 0, 1
FROM category_workflows w
CROSS JOIN (
    SELECT 'IN_PROGRESS' AS status, 'Resume work' AS label
    UNION ALL SELECT 'PENDING_EMPLOYEE', 'Request information'
    UNION ALL SELECT 'RESOLVED', 'Resolve'
) AS t
WHERE NOT EXISTS (
    SELECT 1 FROM category_workflows x
    WHERE x.tenant_id = w.tenant_id AND x.category_id = w.category_id
      AND x.from_status = 'ESCALATED' AND x.to_status = t.status);

-- NEW must reach the states OPEN could, so a freshly raised ticket is not stuck.
INSERT INTO category_workflows
    (tenant_id, category_id, from_status, to_status, label, requires_comment, requires_reason_code, is_active)
SELECT DISTINCT w.tenant_id, w.category_id, 'NEW', 'OPEN', 'Accept', 0, 0, 1
FROM category_workflows w
WHERE NOT EXISTS (
    SELECT 1 FROM category_workflows x
    WHERE x.tenant_id = w.tenant_id AND x.category_id = w.category_id
      AND x.from_status = 'NEW' AND x.to_status = 'OPEN');

-- SLA policies pause on the renamed status.
UPDATE sla_policies
SET pause_on_statuses_json = '["PENDING_EMPLOYEE"]'
WHERE pause_on_statuses_json LIKE '%PENDING_INFORMATION%'
   OR pause_on_statuses_json LIKE '%PENDING_USER%';

-- --- move users onto the canonical roles ------------------------------------

-- Permissions are mirrored onto the aliases by `seed platform`, and the two
-- targets here (HELPDESK_HEAD, CLIENT_ADMIN) are the wider of each pair, so
-- nobody gains or loses access. Leaving users on deprecated keys would mean the
-- model is only half-adopted.
--
-- The NOT EXISTS guard matters: a user may already hold both the old key and its
-- replacement, and uq_user_roles would reject the duplicate.
UPDATE user_roles ur
JOIN roles old ON old.id = ur.role_id AND old.tenant_id IS NULL AND old.alias_of IS NOT NULL
JOIN roles new ON new.tenant_id IS NULL AND new.role_key = old.alias_of
SET ur.role_id = new.id
WHERE NOT EXISTS (
    SELECT 1 FROM (SELECT * FROM user_roles) existing
    WHERE existing.user_id = ur.user_id AND existing.role_id = new.id);

-- Anyone holding both keeps only the canonical one.
DELETE ur FROM user_roles ur
JOIN roles old ON old.id = ur.role_id AND old.tenant_id IS NULL AND old.alias_of IS NOT NULL
WHERE EXISTS (
    SELECT 1 FROM (SELECT * FROM user_roles) existing
    JOIN roles new ON new.id = existing.role_id
    WHERE existing.user_id = ur.user_id AND new.tenant_id IS NULL AND new.role_key = old.alias_of);
