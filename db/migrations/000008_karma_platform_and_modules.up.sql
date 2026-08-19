-- ---------------------------------------------------------------------------
-- 000008 Karma platform tenancy, agent assignments, modules and subcategories
--
-- Implements the master-prompt model:
--   * Karma is itself a tenant, flagged is_platform, holding internal staff.
--   * A Karma Agent is granted specific client tenants; everything else is
--     invisible to them at the query layer.
--   * Modules (PF, ESIC, Payroll, HR, ...) are a platform catalogue enabled
--     per client, and categories belong to a module.
--   * Tickets carry an explicit subcategory.
--   * The status lifecycle gains ASSIGNED and renames PENDING_USER.
--
-- Design note: internal staff live in a platform tenant rather than carrying a
-- NULL tenant_id. MySQL permits repeated NULLs in a unique index, so nullable
-- tenancy would silently break uq_users_email and let two Karma staff share an
-- address. Keeping tenant_id NOT NULL preserves every existing index, foreign
-- key and tenant-scoped query unchanged.
-- ---------------------------------------------------------------------------

-- --- Karma as a platform tenant --------------------------------------------

ALTER TABLE tenants
    -- Distinguishes Karma's own tenant from a client. Drives branding (internal
    -- users see only the Karma logo) and keeps Karma out of client listings.
    ADD COLUMN is_platform TINYINT(1) NOT NULL DEFAULT 0 AFTER status;

ALTER TABLE tenants
    ADD KEY ix_tenants_platform (is_platform, status);

-- The single Karma tenant. Created here rather than in seed data so that every
-- environment has it before any user or assignment references it.
INSERT INTO tenants
    (public_id, slug, client_code, name, legal_name, status, is_platform,
     timezone, locale, date_format, ticket_prefix)
SELECT
    -- MariaDB 10.4 has no RANDOM_BYTES; UUID() hex is a valid Crockford subset,
    -- and the '01' prefix keeps the leading character inside the ULID
    -- timestamp range so the value parses as a real ULID.
    CONCAT('01', UPPER(SUBSTRING(REPLACE(UUID(), '-', ''), 1, 24))), 'karma', 'KARMA',
    'Karma Management Global Consulting Solutions',
    'Karma Management Global Consulting Solutions Pvt. Ltd.',
    'ACTIVE', 1, 'Asia/Kolkata', 'en-IN', 'dd/MM/yyyy', 'KMG'
WHERE NOT EXISTS (SELECT 1 FROM tenants WHERE slug = 'karma');

-- The down migration leaves the row behind (deleting it would cascade to every
-- internal user), so re-applying has to restore the flag on the surviving row.
UPDATE tenants SET is_platform = 1 WHERE slug = 'karma';

-- --- agent assignments ------------------------------------------------------

-- Which client tenants a Karma Agent may work on. A Karma Super Admin needs no
-- rows here: their access is unrestricted by role, not by assignment.
CREATE TABLE agent_tenant_assignments (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id     CHAR(26)        NOT NULL,
    agent_user_id BIGINT UNSIGNED NOT NULL,
    tenant_id     BIGINT UNSIGNED NOT NULL,
    -- A primary agent is the named owner of the client relationship; the rest
    -- are cover. Reporting groups by this.
    is_primary    TINYINT(1)      NOT NULL DEFAULT 0,
    assigned_by   BIGINT UNSIGNED     NULL,
    assigned_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    revoked_at    DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_agent_assignment_public_id (public_id),
    UNIQUE KEY uq_agent_assignment (agent_user_id, tenant_id),
    -- The hot path: "which clients may this agent see?"
    KEY ix_agent_assignment_agent (agent_user_id, revoked_at),
    KEY ix_agent_assignment_tenant (tenant_id, revoked_at),
    CONSTRAINT fk_agent_assignment_user FOREIGN KEY (agent_user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_agent_assignment_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- --- modules ----------------------------------------------------------------

-- Platform catalogue of compliance modules. Adding Labour Law or Professional
-- Tax is a row here, never a code change.
CREATE TABLE modules (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id   CHAR(26)        NOT NULL,
    module_key  VARCHAR(48)     NOT NULL,
    name        VARCHAR(128)    NOT NULL,
    description VARCHAR(500)        NULL,
    icon        VARCHAR(64)         NULL,
    color       VARCHAR(9)          NULL,
    -- A core module is enabled for every new client; the rest are opt-in.
    is_core     TINYINT(1)      NOT NULL DEFAULT 0,
    is_active   TINYINT(1)      NOT NULL DEFAULT 1,
    sort_order  INT             NOT NULL DEFAULT 0,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_modules_public_id (public_id),
    UNIQUE KEY uq_modules_key (module_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Per-client enablement. Absence of a row means "not enabled".
CREATE TABLE tenant_modules (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    module_id   BIGINT UNSIGNED NOT NULL,
    enabled     TINYINT(1)      NOT NULL DEFAULT 1,
    config_json LONGTEXT            NULL CHECK (config_json IS NULL OR JSON_VALID(config_json)),
    enabled_by  BIGINT UNSIGNED     NULL,
    enabled_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_tenant_module (tenant_id, module_id),
    KEY ix_tenant_modules_enabled (tenant_id, enabled),
    CONSTRAINT fk_tenant_modules_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_tenant_modules_module FOREIGN KEY (module_id) REFERENCES modules (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- A category belongs to a module, so disabling Payroll hides its categories
-- everywhere at once.
ALTER TABLE categories
    ADD COLUMN module_id BIGINT UNSIGNED NULL AFTER tenant_id,
    -- Marks a row as a subcategory of parent_id. Kept explicit rather than
    -- inferred from parent_id being non-null, so a query can filter on it.
    ADD COLUMN is_subcategory TINYINT(1) NOT NULL DEFAULT 0 AFTER parent_id;

ALTER TABLE categories
    ADD KEY ix_categories_module (tenant_id, module_id, is_active),
    ADD KEY ix_categories_subcategory (tenant_id, parent_id, is_subcategory),
    ADD CONSTRAINT fk_categories_module FOREIGN KEY (module_id) REFERENCES modules (id) ON DELETE SET NULL;

-- Existing rows are top-level categories.
UPDATE categories SET is_subcategory = 1 WHERE parent_id IS NOT NULL;

-- --- tickets: subcategory and lifecycle ------------------------------------

ALTER TABLE tickets
    ADD COLUMN subcategory_id BIGINT UNSIGNED NULL AFTER category_id;

ALTER TABLE tickets
    ADD KEY ix_tickets_subcategory (tenant_id, subcategory_id, status),
    ADD CONSTRAINT fk_tickets_subcategory FOREIGN KEY (subcategory_id) REFERENCES categories (id) ON DELETE SET NULL;

-- Lifecycle remap to the specified vocabulary:
--   NEW          -> OPEN
--   PENDING_USER -> PENDING_INFORMATION
-- ASSIGNED is new and has no existing rows to migrate.
UPDATE tickets              SET status = 'OPEN'                WHERE status = 'NEW';
UPDATE tickets              SET status = 'PENDING_INFORMATION' WHERE status = 'PENDING_USER';
UPDATE ticket_status_history SET from_status = 'OPEN'                WHERE from_status = 'NEW';
UPDATE ticket_status_history SET to_status   = 'OPEN'                WHERE to_status   = 'NEW';
UPDATE ticket_status_history SET from_status = 'PENDING_INFORMATION' WHERE from_status = 'PENDING_USER';
UPDATE ticket_status_history SET to_status   = 'PENDING_INFORMATION' WHERE to_status   = 'PENDING_USER';
UPDATE category_workflows    SET from_status = 'OPEN'                WHERE from_status = 'NEW';
UPDATE category_workflows    SET to_status   = 'OPEN'                WHERE to_status   = 'NEW';
UPDATE category_workflows    SET from_status = 'PENDING_INFORMATION' WHERE from_status = 'PENDING_USER';
UPDATE category_workflows    SET to_status   = 'PENDING_INFORMATION' WHERE to_status   = 'PENDING_USER';

-- SLA policies pause on the renamed status.
UPDATE sla_policies
SET pause_on_statuses_json = REPLACE(pause_on_statuses_json, 'PENDING_USER', 'PENDING_INFORMATION')
WHERE pause_on_statuses_json LIKE '%PENDING_USER%';

-- --- role consolidation -----------------------------------------------------

-- The master prompt names five roles. The existing eight are kept as aliases so
-- tokens, saved filters and any integration referencing an old key keep working.
ALTER TABLE roles
    ADD COLUMN alias_of VARCHAR(64) NULL AFTER role_key,
    ADD COLUMN is_deprecated TINYINT(1) NOT NULL DEFAULT 0 AFTER is_system;

ALTER TABLE roles
    ADD KEY ix_roles_alias (alias_of);
