-- Remove the ASSIGNED state from the machine. Tickets already sitting in
-- ASSIGNED are folded back into IN_PROGRESS so none is left in a state the
-- workflow no longer knows about.
UPDATE tickets SET status = 'IN_PROGRESS' WHERE status = 'ASSIGNED';

DELETE FROM category_workflows WHERE from_status = 'ASSIGNED' OR to_status = 'ASSIGNED';
