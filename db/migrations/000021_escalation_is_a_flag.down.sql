-- Escalation becomes a status again. The stage each ticket had reached is lost
-- on the way back, which is the whole reason the up migration exists.
UPDATE tickets
   SET status = 'ESCALATED'
 WHERE escalation_level > 0
   AND status NOT IN ('CLOSED', 'CANCELLED', 'RESOLVED');
