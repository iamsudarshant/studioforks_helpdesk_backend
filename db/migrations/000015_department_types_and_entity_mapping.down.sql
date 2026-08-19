-- ---------------------------------------------------------------------------
-- Reverse of 000015.
--
-- The classification and the entity mapping are data, not structure: rolling
-- back returns every department to the placeholder type and unmaps the
-- entities, which is the state 000013 left behind. The General departments this
-- migration created for clients that had none are removed too, but only when
-- nothing has since been attached to them — deleting a department an entity now
-- points at would lose that mapping rather than restore it.
-- ---------------------------------------------------------------------------

DROP INDEX ix_departments_type ON departments;
DROP INDEX ix_entities_dept_active ON entities;

UPDATE entities SET department_id = NULL WHERE deleted_at IS NULL;

DELETE FROM departments
WHERE code = 'GEN' AND name = 'General' AND type = 'GENERAL'
  AND NOT EXISTS (SELECT 1 FROM entities e WHERE e.department_id = departments.id);

UPDATE departments SET type = 'GENERAL' WHERE deleted_at IS NULL;
