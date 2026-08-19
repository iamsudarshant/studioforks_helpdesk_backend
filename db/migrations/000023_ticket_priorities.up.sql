-- ---------------------------------------------------------------------------
-- 000023 Priorities become data
--
-- Priority was a hardcoded enum in three places at once: the create validator,
-- the update validator and the frontend's constants. Adding a level meant a
-- code change in all three and a release, which is the same problem the
-- category catalogue was built to avoid.
--
-- The column on `tickets` deliberately stays a VARCHAR holding the key. Tickets
-- are the hot table and the historical record: pointing them at a priority row
-- by id would mean a join on every list query, and deleting a priority would
-- orphan or rewrite history. The table is the catalogue; the ticket keeps the
-- word it was raised with.
--
-- `weight` is what orders the list and drives "highest first"; `is_default`
-- picks the value used when a requester does not choose. Both are properties of
-- the level rather than of its name, so renaming Medium to Normal changes one
-- row and nothing else.
-- ---------------------------------------------------------------------------

CREATE TABLE ticket_priorities (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id   CHAR(26)        NOT NULL,
    -- NULL means a platform-wide level every client inherits. A row with a
    -- tenant is that client's own addition.
    tenant_id   BIGINT UNSIGNED     NULL,
    priority_key VARCHAR(32)    NOT NULL,
    name        VARCHAR(64)     NOT NULL,
    description VARCHAR(255)        NULL,
    -- Higher sorts first and escalates sooner. Spaced by ten so a level can be
    -- inserted between two existing ones without renumbering.
    weight      INT             NOT NULL DEFAULT 10,
    colour      VARCHAR(16)         NULL,
    is_default  TINYINT(1)      NOT NULL DEFAULT 0,
    is_active   TINYINT(1)      NOT NULL DEFAULT 1,
    -- A level the product itself relies on cannot be deleted, only renamed or
    -- deactivated: removing it would leave existing tickets naming nothing.
    is_system   TINYINT(1)      NOT NULL DEFAULT 0,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at  DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_ticket_priorities_public_id (public_id),
    UNIQUE KEY uq_ticket_priorities_key (tenant_id, priority_key),
    KEY ix_ticket_priorities_active (tenant_id, is_active, weight),
    CONSTRAINT fk_ticket_priorities_tenant
        FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- The three levels the brief names, plus CRITICAL, which existing tickets
-- already use and which would otherwise become unnameable.
INSERT INTO ticket_priorities
    (public_id, tenant_id, priority_key, name, description, weight, colour, is_default, is_system)
VALUES
    (CONCAT('01', UPPER(SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24))), NULL, 'LOW',      'Low',      'Routine. Handled in turn.',                       10, '#5F6368', 0, 1),
    (CONCAT('01', UPPER(SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24))), NULL, 'MEDIUM',   'Medium',   'The default for a new query.',                     20, '#1A73E8', 1, 1),
    (CONCAT('01', UPPER(SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24))), NULL, 'HIGH',     'High',     'Needs attention ahead of routine work.',           30, '#F9AB00', 0, 1),
    (CONCAT('01', UPPER(SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24))), NULL, 'CRITICAL', 'Critical', 'A statutory deadline or a stopped payment.',       40, '#D93025', 0, 1);
