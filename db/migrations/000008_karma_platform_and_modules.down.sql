ALTER TABLE roles
    DROP INDEX ix_roles_alias,
    DROP COLUMN is_deprecated,
    DROP COLUMN alias_of;

UPDATE sla_policies
SET pause_on_statuses_json = REPLACE(pause_on_statuses_json, 'PENDING_INFORMATION', 'PENDING_USER')
WHERE pause_on_statuses_json LIKE '%PENDING_INFORMATION%';

UPDATE category_workflows    SET from_status = 'PENDING_USER' WHERE from_status = 'PENDING_INFORMATION';
UPDATE category_workflows    SET to_status   = 'PENDING_USER' WHERE to_status   = 'PENDING_INFORMATION';
UPDATE category_workflows    SET from_status = 'NEW'          WHERE from_status = 'OPEN';
UPDATE category_workflows    SET to_status   = 'NEW'          WHERE to_status   = 'OPEN';
UPDATE ticket_status_history SET from_status = 'PENDING_USER' WHERE from_status = 'PENDING_INFORMATION';
UPDATE ticket_status_history SET to_status   = 'PENDING_USER' WHERE to_status   = 'PENDING_INFORMATION';
UPDATE ticket_status_history SET from_status = 'NEW'          WHERE from_status = 'OPEN';
UPDATE ticket_status_history SET to_status   = 'NEW'          WHERE to_status   = 'OPEN';
-- ASSIGNED has no pre-migration equivalent; fold it back into IN_PROGRESS.
UPDATE tickets SET status = 'IN_PROGRESS'  WHERE status = 'ASSIGNED';
UPDATE tickets SET status = 'PENDING_USER' WHERE status = 'PENDING_INFORMATION';
UPDATE tickets SET status = 'NEW'          WHERE status = 'OPEN';

ALTER TABLE tickets
    DROP FOREIGN KEY fk_tickets_subcategory;
ALTER TABLE tickets
    DROP INDEX ix_tickets_subcategory,
    DROP COLUMN subcategory_id;

ALTER TABLE categories
    DROP FOREIGN KEY fk_categories_module;
ALTER TABLE categories
    DROP INDEX ix_categories_subcategory,
    DROP INDEX ix_categories_module,
    DROP COLUMN is_subcategory,
    DROP COLUMN module_id;

DROP TABLE IF EXISTS tenant_modules;
DROP TABLE IF EXISTS modules;
DROP TABLE IF EXISTS agent_tenant_assignments;

-- The Karma tenant row is deliberately LEFT IN PLACE.
--
-- users.tenant_id cascades on delete, so removing it would destroy every
-- internal Karma account and their client assignments — a routine one-step
-- rollback would silently wipe the staff directory. Dropping is_platform below
-- demotes the row to an ordinary tenant, which is a harmless residue and far
-- safer than cascading deletes. Remove it by hand if a clean slate is genuinely
-- wanted, after confirming it holds no users.

ALTER TABLE tenants
    DROP INDEX ix_tenants_platform,
    DROP COLUMN is_platform;
