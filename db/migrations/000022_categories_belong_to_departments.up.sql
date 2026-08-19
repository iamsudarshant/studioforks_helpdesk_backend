-- ---------------------------------------------------------------------------
-- 000022 A query category belongs to a statutory line
--
-- `categories.default_department_id` existed and was never populated, so
-- nothing tied a category to a department. The consequence was visible on the
-- one screen that matters: "Raise a ticket" offered all six categories to every
-- client, including the four whose statutory line that client does not run.
-- Ampersand Group has two departments — PF & Compliance and ESIC & Insurance —
-- and was still offered Payroll, IT, HR and General.
--
-- The link already exists in the data, just not as a column: a category carries
-- a module (`modules.module_key`) and a department carries a type
-- (`departments.type`), and the two vocabularies are the same one — PF, ESIC,
-- PAYROLL, HR. This migration writes that correspondence down.
--
-- Matched per tenant, because a category and the department it routes to both
-- belong to one client.
-- ---------------------------------------------------------------------------

UPDATE categories c
JOIN modules m     ON m.id = c.module_id
JOIN departments d ON d.tenant_id = c.tenant_id
                  AND d.type = m.module_key
                  AND d.deleted_at IS NULL
                  AND d.is_active = 1
SET c.default_department_id = d.id
WHERE c.default_department_id IS NULL
  AND c.deleted_at IS NULL;

-- A subcategory routes wherever its parent routes. Done second so it can read
-- the parents this migration has just resolved.
UPDATE categories c
JOIN categories p ON p.id = c.parent_id
SET c.default_department_id = p.default_department_id
WHERE c.is_subcategory = 1
  AND c.default_department_id IS NULL
  AND p.default_department_id IS NOT NULL
  AND c.deleted_at IS NULL;

-- The picker filters on this pair, and so does the routing lookup behind it.
CREATE INDEX ix_categories_department
    ON categories (tenant_id, default_department_id, is_subcategory, is_active);
