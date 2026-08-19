-- ---------------------------------------------------------------------------
-- 000015 Make the department/entity model true of the existing data
--
-- 000013 added departments.type and entities.department_id, but only as
-- columns: every existing department kept the DEFAULT 'GENERAL', and every
-- existing entity kept a NULL department. So the rule the API enforces on write
-- ("an entity belongs to a statutory line") was not yet true of anything
-- already in the database, and the Entities list rendered a blank column.
--
-- This migration makes the data match the model:
--
--   1. Classify each existing department by what it is called. PF and ESIC are
--      the two statutory lines that drive routing; everything else keeps a
--      meaningful type rather than the placeholder.
--   2. Give every entity a department, preferring the client's General line so
--      that an entity is never orphaned. A client with no departments at all
--      gets one created first, because a NULL here is what the API refuses.
--   3. Index the pair the Entities list actually filters on.
--
-- Both steps are idempotent: re-running classifies the same rows the same way
-- and skips entities that already have a department.
-- ---------------------------------------------------------------------------

-- 1. Classify existing departments ------------------------------------------
--
-- Matched on name and code together, because clients name the same line
-- differently ("PF & Compliance", "Provident Fund", "EPFO") but the code is
-- usually the abbreviation.

UPDATE departments
SET type = 'PF'
WHERE deleted_at IS NULL
  AND (name LIKE '%PF%' OR name LIKE '%Provident%' OR name LIKE '%EPF%'
       OR code LIKE '%PF%' OR code LIKE '%EPF%');

UPDATE departments
SET type = 'ESIC'
WHERE deleted_at IS NULL
  AND type = 'GENERAL'
  AND (name LIKE '%ESI%' OR name LIKE '%Insurance%' OR code LIKE '%ESI%');

UPDATE departments
SET type = 'PAYROLL'
WHERE deleted_at IS NULL
  AND type = 'GENERAL'
  AND (name LIKE '%Payroll%' OR name LIKE '%Salary%' OR name LIKE '%Wage%'
       OR code LIKE '%PAY%');

UPDATE departments
SET type = 'HR'
WHERE deleted_at IS NULL
  AND type = 'GENERAL'
  AND (name LIKE '%Human Resource%' OR name LIKE '%People%' OR name = 'HR'
       OR code = 'HR');

UPDATE departments
SET type = 'OTHER'
WHERE deleted_at IS NULL
  AND type = 'GENERAL'
  AND (name LIKE '%Information Technology%' OR name LIKE '%IT %' OR name = 'IT'
       OR name LIKE '%Admin%' OR name LIKE '%Finance%' OR code IN ('IT', 'ADMIN', 'FIN'));

-- 2. Every entity gets a department ------------------------------------------

-- A client that has entities but no department at all would leave step 3 with
-- nothing to point at, so give it a General line first. ULID-shaped public ids
-- keep it indistinguishable from one created through the API.
INSERT INTO departments (public_id, tenant_id, code, name, type, is_active)
SELECT CONCAT('01', UPPER(SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24))),
       t.id, 'GEN', 'General', 'GENERAL', 1
FROM tenants t
WHERE t.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM entities e WHERE e.tenant_id = t.id AND e.deleted_at IS NULL)
  AND NOT EXISTS (SELECT 1 FROM departments d WHERE d.tenant_id = t.id AND d.deleted_at IS NULL);

-- Point every unmapped entity at its client's General line, falling back to
-- whichever department that client created first. Resolved per client in a
-- derived table because MySQL will not read the target of an UPDATE in a
-- correlated subquery.
UPDATE entities e
JOIN (
    SELECT d.tenant_id,
           COALESCE(
               MIN(CASE WHEN d.type = 'GENERAL' THEN d.id END),
               MIN(d.id)
           ) AS department_id
    FROM departments d
    WHERE d.deleted_at IS NULL AND d.is_active = 1
    GROUP BY d.tenant_id
) AS pick ON pick.tenant_id = e.tenant_id
SET e.department_id = pick.department_id
WHERE e.department_id IS NULL AND e.deleted_at IS NULL;

-- 3. The index the Entities list filters on ----------------------------------
--
-- 000013 indexed (tenant_id, department_id). The list also filters on
-- is_active and orders by name, so carry those too.

CREATE INDEX ix_entities_dept_active
    ON entities (tenant_id, department_id, is_active, name);

CREATE INDEX ix_departments_type
    ON departments (tenant_id, type, is_active);
