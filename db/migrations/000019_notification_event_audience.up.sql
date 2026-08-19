-- ---------------------------------------------------------------------------
-- 000019 Notification events know who they are for
--
-- The catalogue was flat, so Preferences → Notifications offered every event to
-- everybody. An employee configuring their notifications was shown
-- `bulk_import.completed`, `report.ready` and the maintenance events — none of
-- which are ever addressed to them — and switching one on did nothing, because
-- nothing was ever going to send it.
--
-- `audience` is a comma-separated set of portals: admin, agents, partner, user.
-- A comma-separated column rather than a join table because the set is tiny,
-- fixed at four values, and only ever read as a whole.
--
-- Everything defaults to the full set, so an event added later is visible until
-- somebody decides otherwise — the failure of an over-broad list is a confusing
-- preference screen, whereas the failure of an over-narrow one is a
-- notification nobody can find the switch for.
-- ---------------------------------------------------------------------------

ALTER TABLE notification_events
    ADD COLUMN audience VARCHAR(64) NOT NULL DEFAULT 'admin,agents,partner,user'
    AFTER event_group;

-- Things that happen to a person's own account. Everyone has an account.
UPDATE notification_events SET audience = 'admin,agents,partner,user'
 WHERE event_key IN (
    'user.account_locked', 'user.group_changed', 'user.login_otp',
    'user.password_reset_link', 'user.temp_password', 'user.username_recovery',
    'user.welcome');

-- The lifecycle of a ticket, as its requester experiences it: raised, answered,
-- resolved, closed, reopened, or waiting on them.
UPDATE notification_events SET audience = 'admin,agents,partner,user'
 WHERE event_key IN (
    'ticket.created', 'ticket.replied', 'ticket.resolved', 'ticket.closed',
    'ticket.reopened', 'ticket.info_requested', 'ticket.reminder_pending_user',
    'ticket.mentioned', 'ticket.status_changed');

-- Desk mechanics. An employee is not assigned tickets, does not escalate them,
-- and is not measured by an SLA — these are addressed to whoever is working the
-- queue, and a partner overseeing it.
UPDATE notification_events SET audience = 'admin,agents,partner'
 WHERE event_key IN ('ticket.assigned', 'ticket.escalated',
                     'ticket.sla_warning', 'ticket.sla_breached');

-- Work the desk runs: an import finishing, a report rendering.
UPDATE notification_events SET audience = 'admin,agents,partner'
 WHERE event_key IN ('bulk_import.completed', 'report.ready');

-- Availability of the platform itself, which is ComplyDesk's own business.
UPDATE notification_events SET audience = 'admin,agents'
 WHERE event_key LIKE 'maintenance.%';
