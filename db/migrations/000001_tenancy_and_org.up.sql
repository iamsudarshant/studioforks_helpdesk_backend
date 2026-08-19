-- ---------------------------------------------------------------------------
-- 000001 Tenancy and organisation structure
--
-- Every tenant-owned table carries tenant_id as the first column of its
-- significant indexes. See docs/ARCHITECTURE.md for the isolation strategy.
-- ---------------------------------------------------------------------------

CREATE TABLE tenants (
    id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id            CHAR(26)        NOT NULL,
    slug                 VARCHAR(64)     NOT NULL,
    name                 VARCHAR(191)    NOT NULL,
    legal_name           VARCHAR(191)        NULL,
    status               VARCHAR(20)     NOT NULL DEFAULT 'ONBOARDING',
    timezone             VARCHAR(64)     NOT NULL DEFAULT 'Asia/Kolkata',
    locale               VARCHAR(16)     NOT NULL DEFAULT 'en-IN',
    date_format          VARCHAR(32)     NOT NULL DEFAULT 'dd/MM/yyyy',
    ticket_prefix        VARCHAR(12)     NOT NULL DEFAULT 'HD',
    contact_email        VARCHAR(191)        NULL,
    contact_phone        VARCHAR(32)         NULL,
    address              TEXT                NULL,
    tax_id               VARCHAR(64)         NULL,
    contract_start       DATE                NULL,
    contract_end         DATE                NULL,
    retention_policy_json LONGTEXT           NULL CHECK (retention_policy_json IS NULL OR JSON_VALID(retention_policy_json)),
    onboarded_at         DATETIME(3)         NULL,
    created_by           BIGINT UNSIGNED     NULL,
    created_at           DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at           DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at           DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_tenants_slug (slug),
    UNIQUE KEY uq_tenants_public_id (public_id),
    KEY ix_tenants_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE tenant_domains (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id    BIGINT UNSIGNED NOT NULL,
    domain       VARCHAR(191)    NOT NULL,
    is_primary   TINYINT(1)      NOT NULL DEFAULT 0,
    ssl_verified TINYINT(1)      NOT NULL DEFAULT 0,
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_tenant_domains_domain (domain),
    KEY ix_tenant_domains_tenant (tenant_id),
    CONSTRAINT fk_tenant_domains_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE tenant_branding (
    tenant_id            BIGINT UNSIGNED NOT NULL,
    logo_path            VARCHAR(512)        NULL,
    logo_dark_path       VARCHAR(512)        NULL,
    favicon_path         VARCHAR(512)        NULL,
    login_bg_path        VARCHAR(512)        NULL,
    email_header_path    VARCHAR(512)        NULL,
    primary_color        VARCHAR(9)      NOT NULL DEFAULT '#1A73E8',
    secondary_color      VARCHAR(9)      NOT NULL DEFAULT '#5F6368',
    accent_color         VARCHAR(9)      NOT NULL DEFAULT '#00897B',
    -- When no tenant logo exists the API falls back to the ComplyDesk logo.
    show_complydesk_logo TINYINT(1)      NOT NULL DEFAULT 1,
    custom_css           TEXT                NULL,
    updated_at           DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (tenant_id),
    CONSTRAINT fk_tenant_branding_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE tenant_features (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    feature_key VARCHAR(64)     NOT NULL,
    enabled     TINYINT(1)      NOT NULL DEFAULT 0,
    config_json LONGTEXT            NULL CHECK (config_json IS NULL OR JSON_VALID(config_json)),
    updated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_tenant_features (tenant_id, feature_key),
    CONSTRAINT fk_tenant_features_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE tenant_settings (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id     BIGINT UNSIGNED NOT NULL,
    setting_key   VARCHAR(96)     NOT NULL,
    value_json    LONGTEXT        NOT NULL CHECK (JSON_VALID(value_json)),
    -- Secrets (SMTP password, SMS key) are stored encrypted; this flags them so
    -- the API masks them on read and never logs them.
    is_secret     TINYINT(1)      NOT NULL DEFAULT 0,
    updated_by    BIGINT UNSIGNED     NULL,
    updated_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_tenant_settings (tenant_id, setting_key),
    CONSTRAINT fk_tenant_settings_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE tenant_settings_history (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    setting_key VARCHAR(96)     NOT NULL,
    old_value   LONGTEXT            NULL,
    new_value   LONGTEXT            NULL,
    actor_id    BIGINT UNSIGNED     NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_tsh_tenant_key (tenant_id, setting_key, created_at),
    CONSTRAINT fk_tsh_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE tenant_onboarding (
    tenant_id            BIGINT UNSIGNED NOT NULL,
    current_step         VARCHAR(48)     NOT NULL DEFAULT 'organisation',
    payload_json         LONGTEXT            NULL CHECK (payload_json IS NULL OR JSON_VALID(payload_json)),
    completed_steps_json LONGTEXT            NULL CHECK (completed_steps_json IS NULL OR JSON_VALID(completed_steps_json)),
    completed_at         DATETIME(3)         NULL,
    updated_by           BIGINT UNSIGNED     NULL,
    updated_at           DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (tenant_id),
    CONSTRAINT fk_tenant_onboarding_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- tenant_id is NULL for a GLOBAL window covering every client.
CREATE TABLE maintenance_windows (
    id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id        CHAR(26)        NOT NULL,
    scope            VARCHAR(16)     NOT NULL DEFAULT 'TENANT',
    tenant_id        BIGINT UNSIGNED     NULL,
    mode             VARCHAR(16)     NOT NULL DEFAULT 'BANNER',
    title            VARCHAR(191)    NOT NULL,
    message          TEXT                NULL,
    starts_at        DATETIME(3)     NOT NULL,
    ends_at          DATETIME(3)         NULL,
    is_active        TINYINT(1)      NOT NULL DEFAULT 1,
    allow_roles_json LONGTEXT            NULL CHECK (allow_roles_json IS NULL OR JSON_VALID(allow_roles_json)),
    created_by       BIGINT UNSIGNED     NULL,
    created_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_maintenance_public_id (public_id),
    KEY ix_maintenance_active (is_active, starts_at, ends_at),
    KEY ix_maintenance_tenant (tenant_id, is_active),
    CONSTRAINT fk_maintenance_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE entities (
    id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id        CHAR(26)        NOT NULL,
    tenant_id        BIGINT UNSIGNED NOT NULL,
    code             VARCHAR(64)     NOT NULL,
    name             VARCHAR(191)    NOT NULL,
    type             VARCHAR(48)         NULL,
    parent_entity_id BIGINT UNSIGNED     NULL,
    address          TEXT                NULL,
    is_active        TINYINT(1)      NOT NULL DEFAULT 1,
    created_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at       DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at       DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_entities_public_id (public_id),
    UNIQUE KEY uq_entities_code (tenant_id, code),
    KEY ix_entities_tenant_active (tenant_id, is_active),
    KEY ix_entities_parent (parent_entity_id),
    CONSTRAINT fk_entities_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_entities_parent FOREIGN KEY (parent_entity_id) REFERENCES entities (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE sites (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id  CHAR(26)        NOT NULL,
    tenant_id  BIGINT UNSIGNED NOT NULL,
    entity_id  BIGINT UNSIGNED     NULL,
    code       VARCHAR(64)     NOT NULL,
    name       VARCHAR(191)    NOT NULL,
    city       VARCHAR(96)         NULL,
    state      VARCHAR(96)         NULL,
    is_active  TINYINT(1)      NOT NULL DEFAULT 1,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_sites_public_id (public_id),
    UNIQUE KEY uq_sites_code (tenant_id, code),
    KEY ix_sites_tenant_entity (tenant_id, entity_id, is_active),
    CONSTRAINT fk_sites_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_sites_entity FOREIGN KEY (entity_id) REFERENCES entities (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE departments (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id    CHAR(26)        NOT NULL,
    tenant_id    BIGINT UNSIGNED NOT NULL,
    code         VARCHAR(64)     NOT NULL,
    name         VARCHAR(191)    NOT NULL,
    head_user_id BIGINT UNSIGNED     NULL,
    is_active    TINYINT(1)      NOT NULL DEFAULT 1,
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at   DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_departments_public_id (public_id),
    UNIQUE KEY uq_departments_code (tenant_id, code),
    KEY ix_departments_tenant_active (tenant_id, is_active),
    CONSTRAINT fk_departments_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
