-- ---------------------------------------------------------------------------
-- 000009 Wire ASSIGNED into the status machine
--
-- Migration 8 introduced ASSIGNED and renamed NEW -> OPEN, but never added the
-- workflow rows for the new state. Assigning a ticket therefore moved it into
-- ASSIGNED and left it stranded: the transition table offered nothing onward,
-- so the API correctly refused every subsequent move.
--
-- These rows give ASSIGNED the same outgoing moves as IN_PROGRESS, and let OPEN
-- reach it. Written per category, because the machine is per-category data.
-- ---------------------------------------------------------------------------

-- OPEN -> ASSIGNED, so picking work up is an explicit, recorded move as well as
-- a side effect of setting an assignee.
INSERT INTO category_workflows
    (tenant_id, category_id, from_status, to_status, label, requires_comment, requires_reason_code, is_active)
SELECT DISTINCT w.tenant_id, w.category_id, 'OPEN', 'ASSIGNED', 'Assign', 0, 0, 1
FROM category_workflows w
WHERE NOT EXISTS (
    SELECT 1 FROM category_workflows x
    WHERE x.tenant_id = w.tenant_id AND x.category_id = w.category_id
      AND x.from_status = 'OPEN' AND x.to_status = 'ASSIGNED');

-- ASSIGNED -> IN_PROGRESS. This one has to be explicit: the copy below clones
-- IN_PROGRESS's outgoing moves, which by definition never include IN_PROGRESS
-- itself, so without this the natural "start working" step is missing.
INSERT INTO category_workflows
    (tenant_id, category_id, from_status, to_status, label, requires_comment, requires_reason_code, is_active)
SELECT DISTINCT w.tenant_id, w.category_id, 'ASSIGNED', 'IN_PROGRESS', 'Start working', 0, 0, 1
FROM category_workflows w
WHERE NOT EXISTS (
    SELECT 1 FROM category_workflows x
    WHERE x.tenant_id = w.tenant_id AND x.category_id = w.category_id
      AND x.from_status = 'ASSIGNED' AND x.to_status = 'IN_PROGRESS');

-- ASSIGNED -> the same destinations IN_PROGRESS already has.
INSERT INTO category_workflows
    (tenant_id, category_id, from_status, to_status, label, requires_comment, requires_reason_code,
     reason_codes_json, is_active)
SELECT w.tenant_id, w.category_id, 'ASSIGNED', w.to_status, w.label,
       w.requires_comment, w.requires_reason_code, w.reason_codes_json, 1
FROM category_workflows w
WHERE w.from_status = 'IN_PROGRESS'
  AND NOT EXISTS (
    SELECT 1 FROM category_workflows x
    WHERE x.tenant_id = w.tenant_id AND x.category_id = w.category_id
      AND x.from_status = 'ASSIGNED' AND x.to_status = w.to_status);

-- A ticket may also be handed back to the queue.
INSERT INTO category_workflows
    (tenant_id, category_id, from_status, to_status, label, requires_comment, requires_reason_code, is_active)
SELECT DISTINCT w.tenant_id, w.category_id, 'ASSIGNED', 'OPEN', 'Return to queue', 1, 0, 1
FROM category_workflows w
WHERE NOT EXISTS (
    SELECT 1 FROM category_workflows x
    WHERE x.tenant_id = w.tenant_id AND x.category_id = w.category_id
      AND x.from_status = 'ASSIGNED' AND x.to_status = 'OPEN');
