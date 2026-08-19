-- ---------------------------------------------------------------------------
-- 000013 Production org model + Help module
--
-- 1. departments.type
--    Every department belongs to a standard statutory line: PF, ESIC, GENERAL.
--    The type drives agent-department routing and the standard department list.
--
-- 2. entities.department_id
--    Every entity must belong to a department (mandatory at the API layer).
--    department_id is stored nullable so existing rows survive the migration;
--    new creates and every update require it.
--
-- 3. Help module
--    faq_articles            — the public/internal knowledge base shown in Help.
--    help_tickets            — "Request Help" raised by any user, answered and
--                              resolved only by Admins and Agents (staff).
--    help_ticket_replies     — the staff/requester thread on a help ticket.
-- ---------------------------------------------------------------------------

-- 1. Standard department types ----------------------------------------------

ALTER TABLE departments
    ADD COLUMN type VARCHAR(48) NOT NULL DEFAULT 'GENERAL' AFTER name;

-- 2. Entity -> department (mandatory mapping) --------------------------------

ALTER TABLE entities
    ADD COLUMN department_id BIGINT UNSIGNED NULL AFTER type;

ALTER TABLE entities
    ADD KEY ix_entities_department (tenant_id, department_id),
    ADD CONSTRAINT fk_entities_department
        FOREIGN KEY (department_id) REFERENCES departments (id) ON DELETE SET NULL;

-- 3. Help module -------------------------------------------------------------

CREATE TABLE faq_articles (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id  CHAR(26)        NOT NULL,
    tenant_id  BIGINT UNSIGNED NOT NULL,
    -- Section/grouping within Help, e.g. ACCOUNTS, TICKETS, PASSWORDS.
    section    VARCHAR(96)     NOT NULL DEFAULT 'GENERAL',
    question   VARCHAR(512)    NOT NULL,
    answer     TEXT            NOT NULL,
    sort_order INT             NOT NULL DEFAULT 0,
    is_active  TINYINT(1)      NOT NULL DEFAULT 1,
    created_by BIGINT UNSIGNED     NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_faq_articles_public_id (public_id),
    KEY ix_faq_articles_tenant_active (tenant_id, is_active, sort_order),
    CONSTRAINT fk_faq_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_faq_author FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- A help ticket is a request from any user for assistance with the product
-- itself (not a compliance ticket). It is answered by staff: admins and agents
-- may reply and resolve; partners and employees may raise and read their own.
CREATE TABLE help_tickets (
    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id      CHAR(26)        NOT NULL,
    tenant_id      BIGINT UNSIGNED NOT NULL,
    -- The client the request relates to, when raised by an agent on behalf of
    -- a client, or by a partner. NULL for a platform-wide request.
    client_id      BIGINT UNSIGNED     NULL,
    requester_id   BIGINT UNSIGNED NOT NULL,
    subject        VARCHAR(255)    NOT NULL,
    -- Category groups requests: BUG, QUESTION, REQUEST, ACCESS.
    category       VARCHAR(48)     NOT NULL DEFAULT 'QUESTION',
    body           TEXT            NOT NULL,
    status         VARCHAR(24)     NOT NULL DEFAULT 'OPEN',
    priority       VARCHAR(16)     NOT NULL DEFAULT 'NORMAL',
    assigned_to    BIGINT UNSIGNED     NULL,
    resolved_by    BIGINT UNSIGNED     NULL,
    resolved_at    DATETIME(3)         NULL,
    created_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at     DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_help_tickets_public_id (public_id),
    KEY ix_help_tickets_tenant_status (tenant_id, status, created_at),
    KEY ix_help_tickets_requester (requester_id, created_at),
    KEY ix_help_tickets_client (client_id, created_at),
    CONSTRAINT fk_ht_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_ht_client FOREIGN KEY (client_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_ht_requester FOREIGN KEY (requester_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_ht_assignee FOREIGN KEY (assigned_to) REFERENCES users (id) ON DELETE SET NULL,
    CONSTRAINT fk_ht_resolver FOREIGN KEY (resolved_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE help_ticket_replies (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    help_ticket_id BIGINT UNSIGNED NOT NULL,
    author_id    BIGINT UNSIGNED NOT NULL,
    -- Direction: the requester talking to staff, or staff answering.
    author_role  VARCHAR(24)     NOT NULL,
    body         TEXT            NOT NULL,
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_htr_ticket (help_ticket_id, created_at),
    CONSTRAINT fk_htr_ticket FOREIGN KEY (help_ticket_id) REFERENCES help_tickets (id) ON DELETE CASCADE,
    CONSTRAINT fk_htr_author FOREIGN KEY (author_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
