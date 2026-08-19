-- ---------------------------------------------------------------------------
-- 000004 Tickets, conversation, timeline, SLA events
-- ---------------------------------------------------------------------------

-- One row per (tenant, prefix, year). Allocated with SELECT ... FOR UPDATE
-- inside the creating transaction so numbers are gapless and race-free.
CREATE TABLE ticket_sequences (
    tenant_id  BIGINT UNSIGNED NOT NULL,
    prefix     VARCHAR(12)     NOT NULL,
    year       SMALLINT        NOT NULL,
    last_value BIGINT UNSIGNED NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, prefix, year),
    CONSTRAINT fk_ticket_sequences_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE tickets (
    id                       BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id                CHAR(26)        NOT NULL,
    tenant_id                BIGINT UNSIGNED NOT NULL,
    ticket_number            VARCHAR(48)     NOT NULL,
    category_id              BIGINT UNSIGNED NOT NULL,
    subject                  VARCHAR(255)    NOT NULL,
    description              MEDIUMTEXT          NULL,
    status                   VARCHAR(24)     NOT NULL DEFAULT 'NEW',
    priority                 VARCHAR(16)     NOT NULL DEFAULT 'MEDIUM',
    source                   VARCHAR(16)     NOT NULL DEFAULT 'PORTAL',
    requester_id             BIGINT UNSIGNED NOT NULL,
    -- Identity frozen at creation so a later profile edit cannot rewrite history.
    requester_snapshot_json  LONGTEXT            NULL CHECK (requester_snapshot_json IS NULL OR JSON_VALID(requester_snapshot_json)),
    entity_id                BIGINT UNSIGNED     NULL,
    site_id                  BIGINT UNSIGNED     NULL,
    department_id            BIGINT UNSIGNED     NULL,
    assignee_id              BIGINT UNSIGNED     NULL,
    custom_fields_json       LONGTEXT            NULL CHECK (custom_fields_json IS NULL OR JSON_VALID(custom_fields_json)),
    sla_policy_id            BIGINT UNSIGNED     NULL,
    first_response_due_at    DATETIME(3)         NULL,
    resolution_due_at        DATETIME(3)         NULL,
    first_responded_at       DATETIME(3)         NULL,
    resolved_at              DATETIME(3)         NULL,
    closed_at                DATETIME(3)         NULL,
    reopened_count           INT             NOT NULL DEFAULT 0,
    last_reopened_at         DATETIME(3)         NULL,
    escalation_level         INT             NOT NULL DEFAULT 0,
    is_sla_breached          TINYINT(1)      NOT NULL DEFAULT 0,
    sla_paused_at            DATETIME(3)         NULL,
    sla_paused_total_mins    INT             NOT NULL DEFAULT 0,
    csat_score               TINYINT             NULL,
    csat_comment             VARCHAR(1000)       NULL,
    parent_ticket_id         BIGINT UNSIGNED     NULL,
    last_activity_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    created_by               BIGINT UNSIGNED     NULL,
    updated_by               BIGINT UNSIGNED     NULL,
    created_at               DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at               DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at               DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_tickets_public_id (public_id),
    UNIQUE KEY uq_tickets_number (tenant_id, ticket_number),
    -- Composite indexes lead with tenant_id; every read path is tenant-scoped.
    KEY ix_tickets_tenant_status (tenant_id, status, created_at),
    KEY ix_tickets_tenant_assignee (tenant_id, assignee_id, status),
    KEY ix_tickets_tenant_requester (tenant_id, requester_id, status),
    KEY ix_tickets_tenant_category (tenant_id, category_id, status),
    KEY ix_tickets_tenant_entity (tenant_id, entity_id, status),
    KEY ix_tickets_tenant_department (tenant_id, department_id, status),
    KEY ix_tickets_tenant_site (tenant_id, site_id, status),
    KEY ix_tickets_sla_due (tenant_id, status, resolution_due_at),
    KEY ix_tickets_frt_due (tenant_id, first_responded_at, first_response_due_at),
    KEY ix_tickets_activity (tenant_id, last_activity_at),
    KEY ix_tickets_created (tenant_id, created_at),
    FULLTEXT KEY ft_tickets_search (subject, description),
    CONSTRAINT fk_tickets_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_tickets_category FOREIGN KEY (category_id) REFERENCES categories (id),
    CONSTRAINT fk_tickets_requester FOREIGN KEY (requester_id) REFERENCES users (id),
    CONSTRAINT fk_tickets_assignee FOREIGN KEY (assignee_id) REFERENCES users (id) ON DELETE SET NULL,
    CONSTRAINT fk_tickets_entity FOREIGN KEY (entity_id) REFERENCES entities (id) ON DELETE SET NULL,
    CONSTRAINT fk_tickets_site FOREIGN KEY (site_id) REFERENCES sites (id) ON DELETE SET NULL,
    CONSTRAINT fk_tickets_department FOREIGN KEY (department_id) REFERENCES departments (id) ON DELETE SET NULL,
    CONSTRAINT fk_tickets_sla FOREIGN KEY (sla_policy_id) REFERENCES sla_policies (id) ON DELETE SET NULL,
    CONSTRAINT fk_tickets_parent FOREIGN KEY (parent_ticket_id) REFERENCES tickets (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE ticket_conversations (
    id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id        CHAR(26)        NOT NULL,
    tenant_id        BIGINT UNSIGNED NOT NULL,
    ticket_id        BIGINT UNSIGNED NOT NULL,
    author_id        BIGINT UNSIGNED     NULL,
    author_role      VARCHAR(48)         NULL,
    -- INTERNAL notes are filtered at the query level, never in the handler.
    visibility       VARCHAR(16)     NOT NULL DEFAULT 'PUBLIC',
    body_html        MEDIUMTEXT          NULL,
    body_text        MEDIUMTEXT          NULL,
    is_system        TINYINT(1)      NOT NULL DEFAULT 0,
    in_reply_to_id   BIGINT UNSIGNED     NULL,
    mentions_json    LONGTEXT            NULL CHECK (mentions_json IS NULL OR JSON_VALID(mentions_json)),
    email_message_id VARCHAR(255)        NULL,
    edited_at        DATETIME(3)         NULL,
    created_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    deleted_at       DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_conversations_public_id (public_id),
    KEY ix_conversations_ticket (tenant_id, ticket_id, created_at),
    KEY ix_conversations_visibility (tenant_id, ticket_id, visibility, created_at),
    KEY ix_conversations_email (email_message_id),
    CONSTRAINT fk_conversations_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_conversations_ticket FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE CASCADE,
    CONSTRAINT fk_conversations_author FOREIGN KEY (author_id) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE ticket_conversation_reads (
    conversation_id BIGINT UNSIGNED NOT NULL,
    user_id         BIGINT UNSIGNED NOT NULL,
    read_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (conversation_id, user_id),
    KEY ix_conv_reads_user (user_id),
    CONSTRAINT fk_conv_reads_conv FOREIGN KEY (conversation_id) REFERENCES ticket_conversations (id) ON DELETE CASCADE,
    CONSTRAINT fk_conv_reads_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE ticket_status_history (
    id                        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id                 BIGINT UNSIGNED NOT NULL,
    ticket_id                 BIGINT UNSIGNED NOT NULL,
    from_status               VARCHAR(24)         NULL,
    to_status                 VARCHAR(24)     NOT NULL,
    reason_code               VARCHAR(64)         NULL,
    comment                   TEXT                NULL,
    actor_id                  BIGINT UNSIGNED     NULL,
    duration_in_previous_secs BIGINT              NULL,
    created_at                DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_status_history_ticket (tenant_id, ticket_id, created_at),
    CONSTRAINT fk_status_history_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_status_history_ticket FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE ticket_assignments (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id     BIGINT UNSIGNED NOT NULL,
    ticket_id     BIGINT UNSIGNED NOT NULL,
    from_user_id  BIGINT UNSIGNED     NULL,
    to_user_id    BIGINT UNSIGNED     NULL,
    from_department_id BIGINT UNSIGNED NULL,
    to_department_id   BIGINT UNSIGNED NULL,
    assignment_type VARCHAR(16)   NOT NULL DEFAULT 'ASSIGN',
    reason        VARCHAR(500)        NULL,
    actor_id      BIGINT UNSIGNED     NULL,
    created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_assignments_ticket (tenant_id, ticket_id, created_at),
    KEY ix_assignments_to_user (tenant_id, to_user_id, created_at),
    CONSTRAINT fk_assignments_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_assignments_ticket FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Append-only. The API never exposes an update or delete for this table.
CREATE TABLE ticket_timeline (
    id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id          CHAR(26)        NOT NULL,
    tenant_id          BIGINT UNSIGNED NOT NULL,
    ticket_id          BIGINT UNSIGNED NOT NULL,
    event_type         VARCHAR(48)     NOT NULL,
    actor_id           BIGINT UNSIGNED     NULL,
    actor_name_snapshot VARCHAR(191)       NULL,
    actor_role         VARCHAR(48)         NULL,
    -- Internal-only events are hidden from employee/partner views but never removed.
    visibility         VARCHAR(16)     NOT NULL DEFAULT 'PUBLIC',
    summary            VARCHAR(500)    NOT NULL,
    detail_json        LONGTEXT            NULL CHECK (detail_json IS NULL OR JSON_VALID(detail_json)),
    created_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_timeline_public_id (public_id),
    KEY ix_timeline_ticket (tenant_id, ticket_id, created_at),
    KEY ix_timeline_type (tenant_id, event_type, created_at),
    CONSTRAINT fk_timeline_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_timeline_ticket FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE ticket_watchers (
    tenant_id  BIGINT UNSIGNED NOT NULL,
    ticket_id  BIGINT UNSIGNED NOT NULL,
    user_id    BIGINT UNSIGNED NOT NULL,
    reason     VARCHAR(64)         NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (ticket_id, user_id),
    KEY ix_watchers_user (tenant_id, user_id),
    CONSTRAINT fk_watchers_ticket FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE CASCADE,
    CONSTRAINT fk_watchers_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE ticket_sla_events (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id     BIGINT UNSIGNED NOT NULL,
    ticket_id     BIGINT UNSIGNED NOT NULL,
    sla_policy_id BIGINT UNSIGNED     NULL,
    event         VARCHAR(24)     NOT NULL,
    level         INT             NOT NULL DEFAULT 0,
    occurred_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_sla_event_once (ticket_id, event, level),
    KEY ix_sla_events_ticket (tenant_id, ticket_id, occurred_at),
    CONSTRAINT fk_sla_events_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_sla_events_ticket FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE ticket_feedback (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id  BIGINT UNSIGNED NOT NULL,
    ticket_id  BIGINT UNSIGNED NOT NULL,
    user_id    BIGINT UNSIGNED NOT NULL,
    score      TINYINT         NOT NULL,
    comment    VARCHAR(1000)       NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_feedback_ticket_user (ticket_id, user_id),
    KEY ix_feedback_tenant (tenant_id, created_at),
    CONSTRAINT fk_feedback_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_feedback_ticket FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE CASCADE,
    CONSTRAINT fk_feedback_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE saved_views (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id    CHAR(26)        NOT NULL,
    tenant_id    BIGINT UNSIGNED NOT NULL,
    user_id      BIGINT UNSIGNED NOT NULL,
    name         VARCHAR(128)    NOT NULL,
    resource     VARCHAR(32)     NOT NULL DEFAULT 'TICKETS',
    filters_json LONGTEXT            NULL CHECK (filters_json IS NULL OR JSON_VALID(filters_json)),
    columns_json LONGTEXT            NULL CHECK (columns_json IS NULL OR JSON_VALID(columns_json)),
    is_shared    TINYINT(1)      NOT NULL DEFAULT 0,
    is_default   TINYINT(1)      NOT NULL DEFAULT 0,
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_saved_views_public_id (public_id),
    KEY ix_saved_views_user (tenant_id, user_id, resource),
    KEY ix_saved_views_shared (tenant_id, resource, is_shared),
    CONSTRAINT fk_saved_views_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_saved_views_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
