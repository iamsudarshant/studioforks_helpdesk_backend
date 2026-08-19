-- ---------------------------------------------------------------------------
-- 000006 Notifications, reports, audit, analytics, bulk import, metrics
-- ---------------------------------------------------------------------------

CREATE TABLE notification_events (
    event_key    VARCHAR(64)  NOT NULL,
    event_group  VARCHAR(32)  NOT NULL,
    description  VARCHAR(255) NOT NULL,
    variables_json LONGTEXT       NULL CHECK (variables_json IS NULL OR JSON_VALID(variables_json)),
    default_channels_json LONGTEXT NULL CHECK (default_channels_json IS NULL OR JSON_VALID(default_channels_json)),
    PRIMARY KEY (event_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- tenant_id NULL is the system default template used when a tenant has no override.
CREATE TABLE notification_templates (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id  CHAR(26)        NOT NULL,
    tenant_id  BIGINT UNSIGNED     NULL,
    event_key  VARCHAR(64)     NOT NULL,
    channel    VARCHAR(16)     NOT NULL,
    subject    VARCHAR(255)        NULL,
    body_html  MEDIUMTEXT          NULL,
    body_text  MEDIUMTEXT          NULL,
    is_active  TINYINT(1)      NOT NULL DEFAULT 1,
    updated_by BIGINT UNSIGNED     NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_templates_public_id (public_id),
    UNIQUE KEY uq_templates (tenant_id, event_key, channel),
    KEY ix_templates_event (event_key, channel),
    CONSTRAINT fk_templates_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_templates_event FOREIGN KEY (event_key) REFERENCES notification_events (event_key) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE tenant_notification_settings (
    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id      BIGINT UNSIGNED NOT NULL,
    event_key      VARCHAR(64)     NOT NULL,
    channel        VARCHAR(16)     NOT NULL,
    enabled        TINYINT(1)      NOT NULL DEFAULT 1,
    updated_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_tenant_notif_settings (tenant_id, event_key, channel),
    CONSTRAINT fk_tns_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE notification_preferences (
    id        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id BIGINT UNSIGNED NOT NULL,
    user_id   BIGINT UNSIGNED NOT NULL,
    event_key VARCHAR(64)     NOT NULL,
    channel   VARCHAR(16)     NOT NULL,
    enabled   TINYINT(1)      NOT NULL DEFAULT 1,
    digest    VARCHAR(16)     NOT NULL DEFAULT 'NONE',
    muted_until DATETIME(3)       NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_notif_prefs (user_id, event_key, channel),
    KEY ix_notif_prefs_tenant (tenant_id, user_id),
    CONSTRAINT fk_notif_prefs_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_notif_prefs_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE notifications (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id   CHAR(26)        NOT NULL,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    user_id     BIGINT UNSIGNED NOT NULL,
    event_key   VARCHAR(64)     NOT NULL,
    channel     VARCHAR(16)     NOT NULL DEFAULT 'IN_APP',
    title       VARCHAR(255)    NOT NULL,
    body        TEXT                NULL,
    link        VARCHAR(512)        NULL,
    entity_type VARCHAR(32)         NULL,
    entity_id   BIGINT UNSIGNED     NULL,
    status      VARCHAR(16)     NOT NULL DEFAULT 'QUEUED',
    attempts    INT             NOT NULL DEFAULT 0,
    error       VARCHAR(500)        NULL,
    sent_at     DATETIME(3)         NULL,
    read_at     DATETIME(3)         NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_notifications_public_id (public_id),
    KEY ix_notifications_user_unread (tenant_id, user_id, read_at, created_at),
    KEY ix_notifications_status (status, created_at),
    CONSTRAINT fk_notifications_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_notifications_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Transactional outbox: events are written in the same transaction as the state
-- change, then published by the worker. Nothing dispatches inline.
CREATE TABLE outbox_events (
    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id      BIGINT UNSIGNED NOT NULL,
    aggregate_type VARCHAR(32)     NOT NULL,
    aggregate_id   BIGINT UNSIGNED NOT NULL,
    event_key      VARCHAR(64)     NOT NULL,
    payload_json   LONGTEXT        NOT NULL CHECK (JSON_VALID(payload_json)),
    attempts       INT             NOT NULL DEFAULT 0,
    last_error     VARCHAR(500)        NULL,
    available_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    published_at   DATETIME(3)         NULL,
    created_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_outbox_pending (published_at, available_at, id),
    KEY ix_outbox_tenant (tenant_id, created_at),
    CONSTRAINT fk_outbox_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE report_definitions (
    id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id         CHAR(26)        NOT NULL,
    tenant_id         BIGINT UNSIGNED     NULL,
    definition_key    VARCHAR(64)     NOT NULL,
    name              VARCHAR(191)    NOT NULL,
    description       VARCHAR(500)        NULL,
    report_type       VARCHAR(16)     NOT NULL DEFAULT 'STANDARD',
    resource          VARCHAR(32)     NOT NULL DEFAULT 'TICKETS',
    -- Whitelisted column/filter/group-by description. Never raw SQL.
    config_json       LONGTEXT        NOT NULL CHECK (JSON_VALID(config_json)),
    visible_to_roles_json LONGTEXT        NULL CHECK (visible_to_roles_json IS NULL OR JSON_VALID(visible_to_roles_json)),
    created_by        BIGINT UNSIGNED     NULL,
    created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_report_defs_public_id (public_id),
    UNIQUE KEY uq_report_defs_key (tenant_id, definition_key),
    CONSTRAINT fk_report_defs_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE report_jobs (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id     CHAR(26)        NOT NULL,
    tenant_id     BIGINT UNSIGNED NOT NULL,
    definition_id BIGINT UNSIGNED     NULL,
    definition_key VARCHAR(64)        NULL,
    params_json   LONGTEXT            NULL CHECK (params_json IS NULL OR JSON_VALID(params_json)),
    format        VARCHAR(8)      NOT NULL DEFAULT 'XLSX',
    status        VARCHAR(16)     NOT NULL DEFAULT 'QUEUED',
    progress      TINYINT         NOT NULL DEFAULT 0,
    row_count     BIGINT              NULL,
    document_id   BIGINT UNSIGNED     NULL,
    requested_by  BIGINT UNSIGNED     NULL,
    started_at    DATETIME(3)         NULL,
    finished_at   DATETIME(3)         NULL,
    error         VARCHAR(500)        NULL,
    created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_report_jobs_public_id (public_id),
    KEY ix_report_jobs_tenant (tenant_id, status, created_at),
    KEY ix_report_jobs_requester (tenant_id, requested_by, created_at),
    CONSTRAINT fk_report_jobs_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_report_jobs_document FOREIGN KEY (document_id) REFERENCES documents (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE report_schedules (
    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id      CHAR(26)        NOT NULL,
    tenant_id      BIGINT UNSIGNED NOT NULL,
    definition_id  BIGINT UNSIGNED NOT NULL,
    cron           VARCHAR(64)     NOT NULL,
    timezone       VARCHAR(64)     NOT NULL DEFAULT 'Asia/Kolkata',
    format         VARCHAR(8)      NOT NULL DEFAULT 'XLSX',
    params_json    LONGTEXT            NULL CHECK (params_json IS NULL OR JSON_VALID(params_json)),
    recipients_json LONGTEXT           NULL CHECK (recipients_json IS NULL OR JSON_VALID(recipients_json)),
    is_active      TINYINT(1)      NOT NULL DEFAULT 1,
    last_run_at    DATETIME(3)         NULL,
    next_run_at    DATETIME(3)         NULL,
    created_by     BIGINT UNSIGNED     NULL,
    created_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_report_schedules_public_id (public_id),
    KEY ix_report_schedules_due (is_active, next_run_at),
    CONSTRAINT fk_report_schedules_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_report_schedules_def FOREIGN KEY (definition_id) REFERENCES report_definitions (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Append-only. The application DB user must not hold UPDATE/DELETE on this
-- table; row_hash chains to prev_hash for tamper evidence.
CREATE TABLE audit_logs (
    id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id           BIGINT UNSIGNED     NULL,
    actor_id            BIGINT UNSIGNED     NULL,
    actor_role          VARCHAR(48)         NULL,
    actor_name_snapshot VARCHAR(191)        NULL,
    action              VARCHAR(96)     NOT NULL,
    entity_type         VARCHAR(48)         NULL,
    entity_id           BIGINT UNSIGNED     NULL,
    entity_public_id    CHAR(26)            NULL,
    before_json         LONGTEXT            NULL,
    after_json          LONGTEXT            NULL,
    ip                  VARCHAR(45)         NULL,
    user_agent          VARCHAR(255)        NULL,
    device_info         VARCHAR(255)        NULL,
    portal              VARCHAR(16)         NULL,
    request_id          VARCHAR(64)         NULL,
    cross_tenant        TINYINT(1)      NOT NULL DEFAULT 0,
    prev_hash           CHAR(64)            NULL,
    row_hash            CHAR(64)            NULL,
    created_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_audit_tenant_created (tenant_id, created_at),
    KEY ix_audit_actor (tenant_id, actor_id, created_at),
    KEY ix_audit_entity (tenant_id, entity_type, entity_id, created_at),
    KEY ix_audit_action (tenant_id, action, created_at),
    KEY ix_audit_request (request_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE login_activity (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id       BIGINT UNSIGNED     NULL,
    user_id         BIGINT UNSIGNED     NULL,
    portal          VARCHAR(16)         NULL,
    identifier_used VARCHAR(191)        NULL,
    result          VARCHAR(24)     NOT NULL,
    ip              VARCHAR(45)         NULL,
    user_agent      VARCHAR(255)        NULL,
    session_id      BIGINT UNSIGNED     NULL,
    created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_login_activity_user (tenant_id, user_id, created_at),
    KEY ix_login_activity_result (tenant_id, result, created_at),
    KEY ix_login_activity_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Powers "select a user and see everything they did on the portal".
CREATE TABLE user_activity (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id     BIGINT UNSIGNED NOT NULL,
    user_id       BIGINT UNSIGNED NOT NULL,
    action        VARCHAR(96)     NOT NULL,
    resource_type VARCHAR(48)         NULL,
    resource_id   BIGINT UNSIGNED     NULL,
    resource_label VARCHAR(191)       NULL,
    portal        VARCHAR(16)         NULL,
    ip            VARCHAR(45)         NULL,
    meta_json     LONGTEXT            NULL CHECK (meta_json IS NULL OR JSON_VALID(meta_json)),
    duration_ms   INT                 NULL,
    created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_user_activity_user (tenant_id, user_id, created_at),
    KEY ix_user_activity_action (tenant_id, action, created_at),
    KEY ix_user_activity_created (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE bulk_import_jobs (
    id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id          CHAR(26)        NOT NULL,
    tenant_id          BIGINT UNSIGNED NOT NULL,
    job_type           VARCHAR(24)     NOT NULL DEFAULT 'USERS',
    mode               VARCHAR(16)     NOT NULL DEFAULT 'VALIDATE',
    file_document_id   BIGINT UNSIGNED     NULL,
    errors_document_id BIGINT UNSIGNED     NULL,
    credentials_document_id BIGINT UNSIGNED NULL,
    credentials_expire_at DATETIME(3)      NULL,
    status             VARCHAR(16)     NOT NULL DEFAULT 'QUEUED',
    progress           TINYINT         NOT NULL DEFAULT 0,
    total_rows         INT             NOT NULL DEFAULT 0,
    valid_rows         INT             NOT NULL DEFAULT 0,
    imported_rows      INT             NOT NULL DEFAULT 0,
    failed_rows        INT             NOT NULL DEFAULT 0,
    options_json       LONGTEXT            NULL CHECK (options_json IS NULL OR JSON_VALID(options_json)),
    requested_by       BIGINT UNSIGNED     NULL,
    started_at         DATETIME(3)         NULL,
    finished_at        DATETIME(3)         NULL,
    error              VARCHAR(500)        NULL,
    created_at         DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_bulk_jobs_public_id (public_id),
    KEY ix_bulk_jobs_tenant (tenant_id, status, created_at),
    CONSTRAINT fk_bulk_jobs_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE bulk_import_errors (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id  BIGINT UNSIGNED NOT NULL,
    job_id     BIGINT UNSIGNED NOT NULL,
    row_number INT             NOT NULL,
    column_name VARCHAR(64)        NULL,
    value      VARCHAR(255)        NULL,
    error_code VARCHAR(48)     NOT NULL,
    message    VARCHAR(500)    NOT NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_bulk_errors_job (job_id, row_number),
    KEY ix_bulk_errors_code (job_id, error_code),
    CONSTRAINT fk_bulk_errors_job FOREIGN KEY (job_id) REFERENCES bulk_import_jobs (id) ON DELETE CASCADE,
    CONSTRAINT fk_bulk_errors_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Pre-aggregated so dashboards never full-scan tickets.
CREATE TABLE metrics_daily (
    id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id      BIGINT UNSIGNED NOT NULL,
    metric_date    DATE            NOT NULL,
    entity_id      BIGINT UNSIGNED     NULL,
    department_id  BIGINT UNSIGNED     NULL,
    category_id    BIGINT UNSIGNED     NULL,
    assignee_id    BIGINT UNSIGNED     NULL,
    created_count  INT             NOT NULL DEFAULT 0,
    resolved_count INT             NOT NULL DEFAULT 0,
    closed_count   INT             NOT NULL DEFAULT 0,
    reopened_count INT             NOT NULL DEFAULT 0,
    escalated_count INT            NOT NULL DEFAULT 0,
    breached_count INT             NOT NULL DEFAULT 0,
    frt_total_mins BIGINT          NOT NULL DEFAULT 0,
    frt_samples    INT             NOT NULL DEFAULT 0,
    art_total_mins BIGINT          NOT NULL DEFAULT 0,
    art_samples    INT             NOT NULL DEFAULT 0,
    csat_total     INT             NOT NULL DEFAULT 0,
    csat_samples   INT             NOT NULL DEFAULT 0,
    updated_at     DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_metrics_daily (tenant_id, metric_date, entity_id, department_id, category_id, assignee_id),
    KEY ix_metrics_daily_tenant_date (tenant_id, metric_date),
    CONSTRAINT fk_metrics_daily_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE idempotency_keys (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id     BIGINT UNSIGNED NOT NULL,
    user_id       BIGINT UNSIGNED     NULL,
    idem_key      VARCHAR(128)    NOT NULL,
    endpoint      VARCHAR(191)    NOT NULL,
    request_hash  CHAR(64)        NOT NULL,
    response_json LONGTEXT            NULL,
    status_code   INT                 NULL,
    created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    expires_at    DATETIME(3)     NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_idempotency (tenant_id, idem_key, endpoint),
    KEY ix_idempotency_expiry (expires_at),
    CONSTRAINT fk_idempotency_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
