-- ---------------------------------------------------------------------------
-- 000024 The ticket lifecycle becomes the five states people actually use
--
-- The workflow carried ten: NEW, OPEN, IN_PROGRESS, PENDING_EMPLOYEE,
-- PENDING_HELPDESK, ESCALATED, RESOLVED, CLOSED, REOPENED, CANCELLED. Several
-- were distinctions nobody was making. OPEN and IN_PROGRESS both meant "the
-- desk has it"; RESOLVED and CLOSED both meant "the work is done"; and
-- ESCALATED was not a stage at all, which migration 000021 already dealt with.
--
-- The set is now the five the desk names:
--
--   NEW               nobody has picked it up
--   PENDING_HELPDESK  waiting on the department
--   PENDING_EMPLOYEE  waiting on the client or the employee
--   CLOSED            done
--   REOPENED          done, and then it wasn't
--
-- CANCELLED survives but is not one of the five: a withdrawn ticket is not a
-- stage of the work, it is the absence of it, and it is reached by an explicit
-- "cancel" rather than chosen from a status list.
--
-- `resolved_at` is deliberately untouched. It is what the reopen window counts
-- from and what the satisfaction survey fires on, so a ticket that was RESOLVED
-- keeps the timestamp that made it resolvable — it simply reads as CLOSED now.
-- ---------------------------------------------------------------------------

-- 1. The tickets themselves -------------------------------------------------

UPDATE tickets SET status = 'PENDING_HELPDESK' WHERE status IN ('OPEN', 'IN_PROGRESS');
UPDATE tickets SET status = 'CLOSED'           WHERE status = 'RESOLVED';

-- 2. The transition table, which is what the action bar renders from ---------
--
-- Rewritten rather than edited: the old rows describe a graph with states that
-- no longer exist, and a transition pointing at a dead state is a button that
-- fails when pressed.

DELETE FROM category_workflows
 WHERE from_status IN ('OPEN', 'IN_PROGRESS', 'RESOLVED', 'ESCALATED')
    OR to_status   IN ('OPEN', 'IN_PROGRESS', 'RESOLVED', 'ESCALATED');

-- Every category gets the same five-state graph. Written as a cross join so a
-- client with fifty categories needs no per-category statement.
INSERT INTO category_workflows
    (tenant_id, category_id, from_status, to_status, label, requires_comment, is_active)
SELECT c.tenant_id, c.id, g.from_status, g.to_status, g.label, g.requires_comment, 1
FROM categories c
JOIN (
    SELECT 'NEW'              AS from_status, 'PENDING_HELPDESK' AS to_status, 'Start working'            AS label, 0 AS requires_comment
    UNION ALL SELECT 'NEW',              'PENDING_EMPLOYEE', 'Ask the employee for information', 1
    UNION ALL SELECT 'NEW',              'CANCELLED',        'Cancel',                           1
    UNION ALL SELECT 'PENDING_HELPDESK', 'PENDING_EMPLOYEE', 'Ask the employee for information', 1
    UNION ALL SELECT 'PENDING_HELPDESK', 'CLOSED',           'Close',                            0
    UNION ALL SELECT 'PENDING_HELPDESK', 'CANCELLED',        'Cancel',                           1
    UNION ALL SELECT 'PENDING_EMPLOYEE', 'PENDING_HELPDESK', 'Reply received — resume',          0
    UNION ALL SELECT 'PENDING_EMPLOYEE', 'CLOSED',           'Close',                            0
    UNION ALL SELECT 'PENDING_EMPLOYEE', 'CANCELLED',        'Cancel',                           1
    UNION ALL SELECT 'CLOSED',           'REOPENED',         'Reopen',                           1
    UNION ALL SELECT 'REOPENED',         'PENDING_HELPDESK', 'Resume work',                      0
    UNION ALL SELECT 'REOPENED',         'PENDING_EMPLOYEE', 'Ask the employee for information',  1
    UNION ALL SELECT 'REOPENED',         'CLOSED',           'Close',                            0
) AS g
WHERE c.deleted_at IS NULL
ON DUPLICATE KEY UPDATE
    label = VALUES(label), requires_comment = VALUES(requires_comment), is_active = 1;

-- 3. Saved views filtering on a retired status would now match nothing.
UPDATE saved_views
   SET filters_json = REPLACE(REPLACE(REPLACE(filters_json,
       '"IN_PROGRESS"', '"PENDING_HELPDESK"'),
       '"OPEN"',        '"PENDING_HELPDESK"'),
       '"RESOLVED"',    '"CLOSED"')
 WHERE filters_json LIKE '%"IN_PROGRESS"%'
    OR filters_json LIKE '%"OPEN"%'
    OR filters_json LIKE '%"RESOLVED"%';
