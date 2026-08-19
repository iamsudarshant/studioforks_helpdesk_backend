-- ---------------------------------------------------------------------------
-- 000021 Escalation stops being a status
--
-- Escalating a ticket used to overwrite its status with ESCALATED, which threw
-- away the thing the board is actually worked from. A ticket waiting on the
-- employee and a ticket waiting on the desk both became "Escalated", and the
-- agent picking it up could no longer tell what it was waiting for.
--
-- Whether a ticket is urgent and what stage it has reached are two different
-- questions. `escalation_level` already existed and already answered the first
-- one; the status column should only ever have answered the second.
--
-- This migration repairs the rows that were flattened. The original status is
-- not recoverable — it was overwritten in place — so each one is restored to
-- the best available answer:
--
--   * assigned and not yet resolved  -> IN_PROGRESS, somebody holds it
--   * unassigned                     -> NEW, nobody has picked it up
--
-- Every repaired row keeps an escalation_level of at least 1, so nothing loses
-- its urgency in the process.
-- ---------------------------------------------------------------------------

UPDATE tickets
   SET escalation_level = GREATEST(escalation_level, 1)
 WHERE status = 'ESCALATED';

UPDATE tickets
   SET status = CASE WHEN assignee_id IS NULL THEN 'NEW' ELSE 'IN_PROGRESS' END
 WHERE status = 'ESCALATED';

-- The saved views and routing rules that filtered on the old status would now
-- match nothing. Pointing them at the flag keeps them meaningful.
UPDATE saved_views
   SET filters_json = REPLACE(filters_json, '"ESCALATED"', '"IN_PROGRESS"')
 WHERE filters_json LIKE '%"ESCALATED"%';
