-- ---------------------------------------------------------------------------
-- 000012 Admin sections: ticket-prefix history and partner/agent assignments
--
-- 1. tenant_prefix_history
--    A client's ticket prefix is editable any time, but every change is
--    recorded: who changed it, and from what to what. This is the log the
--    Admin > Client > Ticket prefix panel shows.
--
-- 2. entity_assignments
--    Admin/agent assigns a partner to an entity. The partner then sees that
--    entity's tickets and users only (the assignment is mirrored into
--    user_scopes so every existing scope filter enforces it), and can be given
--    reply rights on that entity without touching their role.
--
-- 3. site_assignments / department_assignments
--    Same pattern for sites and departments: an admin/agent assigns a partner
--    or agent so they only work that location or team.
-- ---------------------------------------------------------------------------

CREATE TABLE tenant_prefix_history (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id  BIGINT UNSIGNED NOT NULL,
    old_prefix VARCHAR(12)     NOT NULL,
    new_prefix VARCHAR(12)     NOT NULL,
    changed_by BIGINT UNSIGNED     NULL,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_tenant_prefix_history_tenant (tenant_id, created_at),
    CONSTRAINT fk_tph_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_tph_user FOREIGN KEY (changed_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE entity_assignments (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id   CHAR(26)        NOT NULL,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    entity_id   BIGINT UNSIGNED NOT NULL,
    user_id     BIGINT UNSIGNED NOT NULL,
    -- Reply rights on this entity, granted per user. A partner holding this may
    -- reply to the entity's tickets even though their role has reply off.
    can_reply   TINYINT(1)      NOT NULL DEFAULT 0,
    assigned_by BIGINT UNSIGNED     NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_entity_assignments (entity_id, user_id),
    KEY ix_entity_assignments_tenant (tenant_id, entity_id),
    KEY ix_entity_assignments_user (user_id),
    CONSTRAINT fk_ea_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_ea_entity FOREIGN KEY (entity_id) REFERENCES entities (id) ON DELETE CASCADE,
    CONSTRAINT fk_ea_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE site_assignments (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id   CHAR(26)        NOT NULL,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    site_id     BIGINT UNSIGNED NOT NULL,
    user_id     BIGINT UNSIGNED NOT NULL,
    assigned_by BIGINT UNSIGNED     NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_site_assignments (site_id, user_id),
    KEY ix_site_assignments_tenant (tenant_id, site_id),
    KEY ix_site_assignments_user (user_id),
    CONSTRAINT fk_sa_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_sa_site FOREIGN KEY (site_id) REFERENCES sites (id) ON DELETE CASCADE,
    CONSTRAINT fk_sa_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE department_assignments (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id   CHAR(26)        NOT NULL,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    department_id BIGINT UNSIGNED NOT NULL,
    user_id     BIGINT UNSIGNED NOT NULL,
    assigned_by BIGINT UNSIGNED     NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_department_assignments (department_id, user_id),
    KEY ix_department_assignments_tenant (tenant_id, department_id),
    KEY ix_department_assignments_user (user_id),
    CONSTRAINT fk_da_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_da_department FOREIGN KEY (department_id) REFERENCES departments (id) ON DELETE CASCADE,
    CONSTRAINT fk_da_user FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
