-- ---------------------------------------------------------------------------
-- Revert to the four-role model and the previous lifecycle.
--
-- The new roles are demoted to aliases rather than deleted: dropping a role row
-- cascades to user_roles and would silently strip the role from everyone
-- holding it. Aliased, those users keep working.
--
-- The lifecycle reversal is lossy in one direction and says so: NEW and OPEN
-- both map back to OPEN, because the four-role vocabulary had no way to express
-- "seen but not yet worked". Re-applying 000011 cannot recover the distinction
-- for rows that already existed.
-- ---------------------------------------------------------------------------

UPDATE roles SET alias_of = NULL, is_deprecated = 0 WHERE tenant_id IS NULL;

UPDATE roles SET name = 'Agent',   portal = 'agents'  WHERE tenant_id IS NULL AND role_key = 'AGENT';
UPDATE roles SET name = 'Partner', portal = 'partner' WHERE tenant_id IS NULL AND role_key = 'PARTNER';

UPDATE roles SET alias_of = 'SUPER_ADMIN', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('KARMA_SUPER_ADMIN', 'HELPDESK_MASTER_ADMIN');

UPDATE roles SET alias_of = 'AGENT', is_deprecated = 1
WHERE tenant_id IS NULL AND role_key IN ('HELPDESK_HEAD', 'HELPDESK_EXECUTIVE', 'KARMA_AGENT', 'HELPDESK_ADMIN');

UPDATE roles SET alias_of = 'PARTNER', is_deprecated = 1
WHERE tenant_id IS NULL
  AND role_key IN ('CLIENT_ADMIN', 'CLIENT_EXECUTIVE', 'CLIENT_MASTER_ADMIN',
                   'PARTNER_EXECUTIVE', 'ENTITY_ADMIN', 'DEPARTMENT_ADMIN');

UPDATE roles r
JOIN roles c ON c.tenant_id IS NULL AND c.role_key = r.alias_of
SET r.portal = c.portal
WHERE r.tenant_id IS NULL AND r.alias_of IS NOT NULL;

-- Lifecycle. OPEN before NEW this time, mirroring the up migration's ordering
-- constraint in reverse.
UPDATE tickets SET status = 'ASSIGNED'            WHERE status = 'OPEN';
UPDATE tickets SET status = 'OPEN'                WHERE status IN ('NEW', 'ESCALATED');
UPDATE tickets SET status = 'PENDING_INFORMATION' WHERE status = 'PENDING_EMPLOYEE';
UPDATE tickets SET status = 'PENDING_DEPT'        WHERE status = 'PENDING_HELPDESK';

UPDATE ticket_status_history SET from_status = 'ASSIGNED' WHERE from_status = 'OPEN';
UPDATE ticket_status_history SET to_status   = 'ASSIGNED' WHERE to_status   = 'OPEN';
UPDATE ticket_status_history SET from_status = 'OPEN' WHERE from_status IN ('NEW', 'ESCALATED');
UPDATE ticket_status_history SET to_status   = 'OPEN' WHERE to_status   IN ('NEW', 'ESCALATED');
UPDATE ticket_status_history SET from_status = 'PENDING_INFORMATION' WHERE from_status = 'PENDING_EMPLOYEE';
UPDATE ticket_status_history SET to_status   = 'PENDING_INFORMATION' WHERE to_status   = 'PENDING_EMPLOYEE';
UPDATE ticket_status_history SET from_status = 'PENDING_DEPT' WHERE from_status = 'PENDING_HELPDESK';
UPDATE ticket_status_history SET to_status   = 'PENDING_DEPT' WHERE to_status   = 'PENDING_HELPDESK';

DELETE FROM category_workflows WHERE from_status = 'ESCALATED' OR to_status = 'ESCALATED';

UPDATE category_workflows SET from_status = 'ASSIGNED' WHERE from_status = 'OPEN';
UPDATE category_workflows SET to_status   = 'ASSIGNED' WHERE to_status   = 'OPEN';
UPDATE category_workflows SET from_status = 'OPEN' WHERE from_status = 'NEW';
UPDATE category_workflows SET to_status   = 'OPEN' WHERE to_status   = 'NEW';
UPDATE category_workflows SET from_status = 'PENDING_INFORMATION' WHERE from_status = 'PENDING_EMPLOYEE';
UPDATE category_workflows SET to_status   = 'PENDING_INFORMATION' WHERE to_status   = 'PENDING_EMPLOYEE';
UPDATE category_workflows SET from_status = 'PENDING_DEPT' WHERE from_status = 'PENDING_HELPDESK';
UPDATE category_workflows SET to_status   = 'PENDING_DEPT' WHERE to_status   = 'PENDING_HELPDESK';

DELETE w FROM category_workflows w
JOIN category_workflows keep
  ON keep.tenant_id = w.tenant_id AND keep.category_id = w.category_id
 AND keep.from_status = w.from_status AND keep.to_status = w.to_status
 AND keep.id < w.id;

DELETE FROM category_workflows WHERE from_status = to_status;

UPDATE sla_policies SET pause_on_statuses_json = '["PENDING_INFORMATION"]'
WHERE pause_on_statuses_json LIKE '%PENDING_EMPLOYEE%';
