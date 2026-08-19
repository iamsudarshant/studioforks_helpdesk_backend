-- ---------------------------------------------------------------------------
-- 000020 The employee lifecycle becomes a first-class transition
--
-- Moving somebody between Active and Ex-Employee was only possible in bulk,
-- through the group-move workflow, which asks about ticket disposition and
-- notification batches — far more than an administrator changing one person's
-- status wants to answer. Doing it one user at a time needed two things the
-- schema did not have:
--
--   1. A handling agent. When an employee leaves, their open queries still
--      belong to somebody, and when one returns there has to be a named point
--      of contact again. `last_working_day` already recorded *when* they left;
--      nothing recorded *who picks it up*.
--
--   2. Somewhere to record the change itself. `status` holds the current value
--      and says nothing about how it got there, so "who made this person an
--      ex-employee, and when" was unanswerable from the user row.
--
-- The agent is a plain FK to users rather than a join table: an employee has
-- exactly one handling agent at a time, and history lives in the audit log.
-- ON DELETE SET NULL, because losing the agent must not lose the employee.
-- ---------------------------------------------------------------------------

ALTER TABLE users
    ADD COLUMN handling_agent_id BIGINT UNSIGNED NULL AFTER user_group_id,
    ADD COLUMN employment_changed_at DATETIME(3) NULL AFTER handling_agent_id,
    ADD COLUMN employment_changed_by BIGINT UNSIGNED NULL AFTER employment_changed_at;

ALTER TABLE users
    ADD CONSTRAINT fk_users_handling_agent
        FOREIGN KEY (handling_agent_id) REFERENCES users (id) ON DELETE SET NULL;

-- "Which employees is this agent responsible for?" is the question the agent's
-- own worklist asks, so it gets the index rather than the reverse direction.
CREATE INDEX ix_users_handling_agent ON users (tenant_id, handling_agent_id, status);

-- An ex-employee already carries a last working day; backfill the audit columns
-- so a row that was moved before this migration does not read as never having
-- changed at all. `updated_at` is the closest true answer available.
UPDATE users
   SET employment_changed_at = updated_at
 WHERE status = 'EX_EMPLOYEE' AND employment_changed_at IS NULL;
