-- ---------------------------------------------------------------------------
-- 000017 PAN is unique among a client's employees, not across everybody
--
-- `uq_users_pan` made a PAN unique per client for every kind of user. That is
-- right for employees — two employees of one client sharing a PAN is a data
-- entry error that would later merge two people's statutory records — but wrong
-- for staff and partners.
--
-- The same person legitimately holds more than one account: an agent who is
-- also a partner at a client they administer, a partner who is also an employee
-- of the same organisation. They have one PAN, because a person has one PAN.
-- The old index made those accounts impossible to create.
--
-- The rule now lives in the application (user.EmployeePANTaken), which can ask
-- "is this an employee?" — a question an index cannot express, because the
-- answer is in user_roles. The index is dropped and replaced with a plain one:
-- the lookups that used it are still fast, and correctness moves to the layer
-- that can state the rule properly.
--
-- UAN and PF stay globally unique per client. Those are issued per person by
-- EPFO and genuinely cannot be shared, whatever role the account holds.
-- ---------------------------------------------------------------------------

ALTER TABLE users DROP INDEX uq_users_pan;

-- Still indexed: the duplicate check reads it on every employee create, and the
-- Users search filters on it.
CREATE INDEX ix_users_pan ON users (tenant_id, pan_number);
