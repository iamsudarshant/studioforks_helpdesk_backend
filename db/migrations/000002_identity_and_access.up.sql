-- ---------------------------------------------------------------------------
-- 000002 Identity, roles, permissions, scopes, sessions
-- ---------------------------------------------------------------------------

CREATE TABLE user_groups (
    id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id         CHAR(26)        NOT NULL,
    tenant_id         BIGINT UNSIGNED NOT NULL,
    group_key         VARCHAR(64)     NOT NULL,
    name              VARCHAR(191)    NOT NULL,
    description       VARCHAR(500)        NULL,
    is_system         TINYINT(1)      NOT NULL DEFAULT 0,
    -- READ_ONLY is what makes the ex-employee group read-only for historic tickets.
    access_mode       VARCHAR(16)     NOT NULL DEFAULT 'FULL',
    grace_period_days INT             NOT NULL DEFAULT 0,
    sla_policy_id     BIGINT UNSIGNED     NULL,
    created_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at        DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_user_groups_public_id (public_id),
    UNIQUE KEY uq_user_groups_key (tenant_id, group_key),
    CONSTRAINT fk_user_groups_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE users (
    id                    BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id             CHAR(26)        NOT NULL,
    tenant_id             BIGINT UNSIGNED NOT NULL,
    employee_code         VARCHAR(64)         NULL,
    username              VARCHAR(96)         NULL,
    first_name            VARCHAR(96)     NOT NULL,
    last_name             VARCHAR(96)         NULL,
    email                 VARCHAR(191)        NULL,
    alt_email             VARCHAR(191)        NULL,
    mobile                VARCHAR(20)         NULL,
    alt_mobile            VARCHAR(20)         NULL,
    pan_number            VARCHAR(16)         NULL,
    uan_number            VARCHAR(20)         NULL,
    pf_number             VARCHAR(40)         NULL,
    esic_number           VARCHAR(24)         NULL,
    date_of_joining       DATE                NULL,
    date_of_birth         DATE                NULL,
    last_working_day      DATE                NULL,
    entity_id             BIGINT UNSIGNED     NULL,
    site_id               BIGINT UNSIGNED     NULL,
    department_id         BIGINT UNSIGNED     NULL,
    designation           VARCHAR(128)        NULL,
    user_group_id         BIGINT UNSIGNED     NULL,
    status                VARCHAR(20)     NOT NULL DEFAULT 'ACTIVE',
    password_hash         VARCHAR(255)        NULL,
    password_algo         VARCHAR(24)     NOT NULL DEFAULT 'argon2id',
    must_change_password  TINYINT(1)      NOT NULL DEFAULT 0,
    password_changed_at   DATETIME(3)         NULL,
    password_expires_at   DATETIME(3)         NULL,
    failed_login_count    INT             NOT NULL DEFAULT 0,
    locked_until          DATETIME(3)         NULL,
    mfa_enabled           TINYINT(1)      NOT NULL DEFAULT 0,
    mfa_secret_enc        VARBINARY(512)      NULL,
    mfa_recovery_json     LONGTEXT            NULL CHECK (mfa_recovery_json IS NULL OR JSON_VALID(mfa_recovery_json)),
    last_login_at         DATETIME(3)         NULL,
    login_count           INT             NOT NULL DEFAULT 0,
    avatar_path           VARCHAR(512)        NULL,
    locale                VARCHAR(16)         NULL,
    timezone              VARCHAR(64)         NULL,
    custom_fields_json    LONGTEXT            NULL CHECK (custom_fields_json IS NULL OR JSON_VALID(custom_fields_json)),
    created_by            BIGINT UNSIGNED     NULL,
    updated_by            BIGINT UNSIGNED     NULL,
    created_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at            DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_users_public_id (public_id),
    -- NULLs repeat freely in a MySQL/MariaDB unique index, which gives the
    -- "unique when present" behaviour these optional identifiers need.
    UNIQUE KEY uq_users_email (tenant_id, email),
    UNIQUE KEY uq_users_employee_code (tenant_id, employee_code),
    UNIQUE KEY uq_users_username (tenant_id, username),
    UNIQUE KEY uq_users_pf (tenant_id, pf_number),
    UNIQUE KEY uq_users_uan (tenant_id, uan_number),
    UNIQUE KEY uq_users_pan (tenant_id, pan_number),
    KEY ix_users_tenant_status (tenant_id, status),
    KEY ix_users_tenant_group (tenant_id, user_group_id),
    KEY ix_users_tenant_entity (tenant_id, entity_id),
    KEY ix_users_tenant_department (tenant_id, department_id),
    KEY ix_users_mobile (tenant_id, mobile),
    KEY ix_users_alt_email (tenant_id, alt_email),
    KEY ix_users_name (tenant_id, first_name, last_name),
    KEY ix_users_lwd (tenant_id, last_working_day),
    CONSTRAINT fk_users_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_users_entity FOREIGN KEY (entity_id) REFERENCES entities (id) ON DELETE SET NULL,
    CONSTRAINT fk_users_site FOREIGN KEY (site_id) REFERENCES sites (id) ON DELETE SET NULL,
    CONSTRAINT fk_users_department FOREIGN KEY (department_id) REFERENCES departments (id) ON DELETE SET NULL,
    CONSTRAINT fk_users_group FOREIGN KEY (user_group_id) REFERENCES user_groups (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

ALTER TABLE departments
    ADD CONSTRAINT fk_departments_head FOREIGN KEY (head_user_id) REFERENCES users (id) ON DELETE SET NULL;

CREATE TABLE password_history (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    user_id       BIGINT UNSIGNED NOT NULL,
    password_hash VARCHAR(255)    NOT NULL,
    created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_password_history_user (user_id, created_at),
    CONSTRAINT fk_password_history_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- tenant_id NULL marks a system role available to every tenant.
CREATE TABLE roles (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id   CHAR(26)        NOT NULL,
    tenant_id   BIGINT UNSIGNED     NULL,
    role_key    VARCHAR(64)     NOT NULL,
    name        VARCHAR(128)    NOT NULL,
    description VARCHAR(500)        NULL,
    portal      VARCHAR(16)     NOT NULL,
    is_system   TINYINT(1)      NOT NULL DEFAULT 0,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_roles_public_id (public_id),
    UNIQUE KEY uq_roles_key (tenant_id, role_key),
    KEY ix_roles_portal (portal)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE permissions (
    permission_key VARCHAR(64)  NOT NULL,
    permission_group VARCHAR(48) NOT NULL,
    description    VARCHAR(255) NOT NULL,
    PRIMARY KEY (permission_key),
    KEY ix_permissions_group (permission_group)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE role_permissions (
    role_id        BIGINT UNSIGNED NOT NULL,
    permission_key VARCHAR(64)     NOT NULL,
    PRIMARY KEY (role_id, permission_key),
    KEY ix_role_permissions_perm (permission_key),
    CONSTRAINT fk_role_permissions_role FOREIGN KEY (role_id) REFERENCES roles (id) ON DELETE CASCADE,
    CONSTRAINT fk_role_permissions_perm FOREIGN KEY (permission_key) REFERENCES permissions (permission_key) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE user_roles (
    user_id    BIGINT UNSIGNED NOT NULL,
    role_id    BIGINT UNSIGNED NOT NULL,
    granted_by BIGINT UNSIGNED     NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (user_id, role_id),
    KEY ix_user_roles_role (role_id),
    CONSTRAINT fk_user_roles_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_user_roles_role FOREIGN KEY (role_id) REFERENCES roles (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- A scoped user with no rows here for a dimension sees nothing on it.
CREATE TABLE user_scopes (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id  BIGINT UNSIGNED NOT NULL,
    user_id    BIGINT UNSIGNED NOT NULL,
    scope_type VARCHAR(16)     NOT NULL,
    scope_id   BIGINT UNSIGNED NOT NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_user_scopes (user_id, scope_type, scope_id),
    KEY ix_user_scopes_tenant (tenant_id, scope_type, scope_id),
    CONSTRAINT fk_user_scopes_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_user_scopes_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE user_group_transfers (
    id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id           BIGINT UNSIGNED NOT NULL,
    batch_id            CHAR(26)        NOT NULL,
    user_id             BIGINT UNSIGNED NOT NULL,
    from_group_id       BIGINT UNSIGNED     NULL,
    to_group_id         BIGINT UNSIGNED NOT NULL,
    last_working_day    DATE                NULL,
    tickets_transferred INT             NOT NULL DEFAULT 0,
    actor_id            BIGINT UNSIGNED     NULL,
    created_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_ugt_batch (tenant_id, batch_id),
    KEY ix_ugt_user (user_id),
    CONSTRAINT fk_ugt_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_ugt_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE sessions (
    id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id          CHAR(26)        NOT NULL,
    tenant_id          BIGINT UNSIGNED NOT NULL,
    user_id            BIGINT UNSIGNED NOT NULL,
    refresh_token_hash CHAR(64)        NOT NULL,
    -- Rotation family: reuse of a rotated token revokes every session in it.
    family_id          CHAR(26)        NOT NULL,
    portal             VARCHAR(16)     NOT NULL,
    ip                 VARCHAR(45)         NULL,
    user_agent         VARCHAR(255)        NULL,
    device_fingerprint VARCHAR(128)        NULL,
    issued_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    expires_at         DATETIME(3)     NOT NULL,
    last_seen_at       DATETIME(3)         NULL,
    revoked_at         DATETIME(3)         NULL,
    revoked_reason     VARCHAR(64)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_sessions_public_id (public_id),
    UNIQUE KEY uq_sessions_token (refresh_token_hash),
    KEY ix_sessions_user_active (user_id, revoked_at, expires_at),
    KEY ix_sessions_family (family_id),
    KEY ix_sessions_tenant (tenant_id),
    CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_sessions_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE password_reset_tokens (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id    BIGINT UNSIGNED NOT NULL,
    user_id      BIGINT UNSIGNED NOT NULL,
    token_hash   CHAR(64)        NOT NULL,
    token_type   VARCHAR(24)     NOT NULL,
    channel      VARCHAR(24)         NULL,
    sent_to      VARCHAR(191)        NULL,
    requested_by BIGINT UNSIGNED     NULL,
    ip           VARCHAR(45)         NULL,
    expires_at   DATETIME(3)     NOT NULL,
    used_at      DATETIME(3)         NULL,
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_reset_token (token_hash),
    KEY ix_reset_user (user_id, used_at, expires_at),
    CONSTRAINT fk_reset_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_reset_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE otp_codes (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    user_id     BIGINT UNSIGNED NOT NULL,
    purpose     VARCHAR(24)     NOT NULL,
    code_hash   CHAR(64)        NOT NULL,
    destination VARCHAR(191)        NULL,
    attempts    INT             NOT NULL DEFAULT 0,
    expires_at  DATETIME(3)     NOT NULL,
    consumed_at DATETIME(3)         NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_otp_user_purpose (user_id, purpose, consumed_at, expires_at),
    CONSTRAINT fk_otp_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_otp_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE api_keys (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id    CHAR(26)        NOT NULL,
    tenant_id    BIGINT UNSIGNED NOT NULL,
    name         VARCHAR(128)    NOT NULL,
    key_prefix   VARCHAR(16)     NOT NULL,
    key_hash     CHAR(64)        NOT NULL,
    scopes_json  LONGTEXT            NULL CHECK (scopes_json IS NULL OR JSON_VALID(scopes_json)),
    created_by   BIGINT UNSIGNED     NULL,
    last_used_at DATETIME(3)         NULL,
    expires_at   DATETIME(3)         NULL,
    revoked_at   DATETIME(3)         NULL,
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_api_keys_public_id (public_id),
    UNIQUE KEY uq_api_keys_hash (key_hash),
    KEY ix_api_keys_tenant (tenant_id, revoked_at),
    CONSTRAINT fk_api_keys_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE user_preferences (
    user_id     BIGINT UNSIGNED NOT NULL,
    theme       VARCHAR(16)     NOT NULL DEFAULT 'system',
    density     VARCHAR(16)     NOT NULL DEFAULT 'comfortable',
    language    VARCHAR(16)     NOT NULL DEFAULT 'en',
    extras_json LONGTEXT            NULL CHECK (extras_json IS NULL OR JSON_VALID(extras_json)),
    updated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (user_id),
    CONSTRAINT fk_user_preferences_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
