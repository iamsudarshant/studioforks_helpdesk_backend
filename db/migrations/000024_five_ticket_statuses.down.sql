-- The finer-grained states are not recoverable: OPEN and IN_PROGRESS were
-- merged into PENDING_HELPDESK and cannot be told apart again, and RESOLVED was
-- merged into CLOSED. This restores the transition graph only.
DELETE FROM category_workflows WHERE from_status = 'REOPENED';
