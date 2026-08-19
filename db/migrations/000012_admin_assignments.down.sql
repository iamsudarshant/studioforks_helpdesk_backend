-- Revert the admin-section assignment tables. Order matters only for the
-- foreign keys: children first.
DROP TABLE IF EXISTS department_assignments;
DROP TABLE IF EXISTS site_assignments;
DROP TABLE IF EXISTS entity_assignments;
DROP TABLE IF EXISTS tenant_prefix_history;
