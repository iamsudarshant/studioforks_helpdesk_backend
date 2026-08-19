-- ---------------------------------------------------------------------------
-- 000003 Query categories, custom fields, workflow, routing, SLA
--
-- Everything the ticket engine branches on lives here as data, so a new query
-- domain (IT, HR, Finance) is a configuration change, not a code change.
-- ---------------------------------------------------------------------------

CREATE TABLE business_hours (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id     CHAR(26)        NOT NULL,
    tenant_id     BIGINT UNSIGNED NOT NULL,
    name          VARCHAR(128)    NOT NULL,
    timezone      VARCHAR(64)     NOT NULL DEFAULT 'Asia/Kolkata',
    -- {"mon":[{"from":"09:30","to":"18:30"}], ...}
    schedule_json LONGTEXT        NOT NULL CHECK (JSON_VALID(schedule_json)),
    -- ["2026-01-26","2026-08-15"]
    holidays_json LONGTEXT            NULL CHECK (holidays_json IS NULL OR JSON_VALID(holidays_json)),
    is_default    TINYINT(1)      NOT NULL DEFAULT 0,
    created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_business_hours_public_id (public_id),
    KEY ix_business_hours_tenant (tenant_id),
    CONSTRAINT fk_business_hours_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE sla_policies (
    id                    BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id             CHAR(26)        NOT NULL,
    tenant_id             BIGINT UNSIGNED NOT NULL,
    name                  VARCHAR(128)    NOT NULL,
    description           VARCHAR(500)        NULL,
    category_id           BIGINT UNSIGNED     NULL,
    priority              VARCHAR(16)         NULL,
    user_group_id         BIGINT UNSIGNED     NULL,
    first_response_mins   INT             NOT NULL DEFAULT 240,
    resolution_mins       INT             NOT NULL DEFAULT 2880,
    business_hours_id     BIGINT UNSIGNED     NULL,
    -- [{"at_percent":75,"notify_roles":["HELPDESK_ADMIN"],"notify_users":[],"reassign_to":null}]
    escalation_json       LONGTEXT            NULL CHECK (escalation_json IS NULL OR JSON_VALID(escalation_json)),
    -- Statuses during which the clock stops; defaults to ["PENDING_USER"].
    pause_on_statuses_json LONGTEXT           NULL CHECK (pause_on_statuses_json IS NULL OR JSON_VALID(pause_on_statuses_json)),
    is_default            TINYINT(1)      NOT NULL DEFAULT 0,
    is_active             TINYINT(1)      NOT NULL DEFAULT 1,
    created_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_sla_public_id (public_id),
    KEY ix_sla_tenant_active (tenant_id, is_active),
    KEY ix_sla_match (tenant_id, category_id, priority, user_group_id),
    CONSTRAINT fk_sla_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_sla_business_hours FOREIGN KEY (business_hours_id) REFERENCES business_hours (id) ON DELETE SET NULL,
    CONSTRAINT fk_sla_group FOREIGN KEY (user_group_id) REFERENCES user_groups (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

ALTER TABLE user_groups
    ADD CONSTRAINT fk_user_groups_sla FOREIGN KEY (sla_policy_id) REFERENCES sla_policies (id) ON DELETE SET NULL;

CREATE TABLE categories (
    id                    BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id             CHAR(26)        NOT NULL,
    tenant_id             BIGINT UNSIGNED NOT NULL,
    category_key          VARCHAR(64)     NOT NULL,
    name                  VARCHAR(191)    NOT NULL,
    description           VARCHAR(500)        NULL,
    parent_id             BIGINT UNSIGNED     NULL,
    ticket_prefix         VARCHAR(12)     NOT NULL,
    icon                  VARCHAR(64)         NULL,
    color                 VARCHAR(9)          NULL,
    sla_policy_id         BIGINT UNSIGNED     NULL,
    default_department_id BIGINT UNSIGNED     NULL,
    -- Profile fields the requester must have populated, e.g. ["pf_number","date_of_joining"]
    requires_fields_json  LONGTEXT            NULL CHECK (requires_fields_json IS NULL OR JSON_VALID(requires_fields_json)),
    is_active             TINYINT(1)      NOT NULL DEFAULT 1,
    sort_order            INT             NOT NULL DEFAULT 0,
    created_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at            DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at            DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_categories_public_id (public_id),
    UNIQUE KEY uq_categories_key (tenant_id, category_key),
    KEY ix_categories_tenant_active (tenant_id, is_active, sort_order),
    KEY ix_categories_parent (parent_id),
    CONSTRAINT fk_categories_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_categories_parent FOREIGN KEY (parent_id) REFERENCES categories (id) ON DELETE SET NULL,
    CONSTRAINT fk_categories_sla FOREIGN KEY (sla_policy_id) REFERENCES sla_policies (id) ON DELETE SET NULL,
    CONSTRAINT fk_categories_department FOREIGN KEY (default_department_id) REFERENCES departments (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

ALTER TABLE sla_policies
    ADD CONSTRAINT fk_sla_category FOREIGN KEY (category_id) REFERENCES categories (id) ON DELETE CASCADE;

CREATE TABLE category_fields (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id       CHAR(26)        NOT NULL,
    tenant_id       BIGINT UNSIGNED NOT NULL,
    category_id     BIGINT UNSIGNED NOT NULL,
    field_key       VARCHAR(64)     NOT NULL,
    label           VARCHAR(191)    NOT NULL,
    field_type      VARCHAR(24)     NOT NULL,
    options_json    LONGTEXT            NULL CHECK (options_json IS NULL OR JSON_VALID(options_json)),
    is_required     TINYINT(1)      NOT NULL DEFAULT 0,
    -- {"pattern":"^\\d+$","min":0,"max":100,"max_size_mb":10,"file_types":["pdf"]}
    validation_json LONGTEXT            NULL CHECK (validation_json IS NULL OR JSON_VALID(validation_json)),
    help_text       VARCHAR(500)        NULL,
    placeholder     VARCHAR(191)        NULL,
    default_value   VARCHAR(500)        NULL,
    visible_to_json LONGTEXT            NULL CHECK (visible_to_json IS NULL OR JSON_VALID(visible_to_json)),
    editable_by_json LONGTEXT           NULL CHECK (editable_by_json IS NULL OR JSON_VALID(editable_by_json)),
    -- {"field":"query_type","operator":"eq","value":"WITHDRAWAL"}
    depends_on_json LONGTEXT            NULL CHECK (depends_on_json IS NULL OR JSON_VALID(depends_on_json)),
    sort_order      INT             NOT NULL DEFAULT 0,
    is_active       TINYINT(1)      NOT NULL DEFAULT 1,
    created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_category_fields_public_id (public_id),
    UNIQUE KEY uq_category_fields_key (category_id, field_key),
    KEY ix_category_fields_cat (tenant_id, category_id, is_active, sort_order),
    CONSTRAINT fk_category_fields_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_category_fields_category FOREIGN KEY (category_id) REFERENCES categories (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- The ticket status machine is a table, never a switch statement.
CREATE TABLE category_workflows (
    id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id            BIGINT UNSIGNED NOT NULL,
    category_id          BIGINT UNSIGNED     NULL,
    from_status          VARCHAR(24)     NOT NULL,
    to_status            VARCHAR(24)     NOT NULL,
    allowed_roles_json   LONGTEXT            NULL CHECK (allowed_roles_json IS NULL OR JSON_VALID(allowed_roles_json)),
    requires_comment     TINYINT(1)      NOT NULL DEFAULT 0,
    requires_reason_code TINYINT(1)      NOT NULL DEFAULT 0,
    reason_codes_json    LONGTEXT            NULL CHECK (reason_codes_json IS NULL OR JSON_VALID(reason_codes_json)),
    auto_after_hours     INT                 NULL,
    label                VARCHAR(96)         NULL,
    is_active            TINYINT(1)      NOT NULL DEFAULT 1,
    created_at           DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at           DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_workflow_transition (tenant_id, category_id, from_status, to_status),
    KEY ix_workflow_lookup (tenant_id, category_id, from_status, is_active),
    CONSTRAINT fk_workflow_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_workflow_category FOREIGN KEY (category_id) REFERENCES categories (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE routing_rules (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id       CHAR(26)        NOT NULL,
    tenant_id       BIGINT UNSIGNED NOT NULL,
    name            VARCHAR(128)    NOT NULL,
    category_id     BIGINT UNSIGNED     NULL,
    priority_order  INT             NOT NULL DEFAULT 100,
    -- [{"field":"entity_id","operator":"in","value":[1,2]}]
    conditions_json LONGTEXT            NULL CHECK (conditions_json IS NULL OR JSON_VALID(conditions_json)),
    action          VARCHAR(32)     NOT NULL,
    target_id       BIGINT UNSIGNED     NULL,
    target_value    VARCHAR(64)         NULL,
    stop_on_match   TINYINT(1)      NOT NULL DEFAULT 0,
    is_active       TINYINT(1)      NOT NULL DEFAULT 1,
    created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_routing_public_id (public_id),
    KEY ix_routing_lookup (tenant_id, is_active, priority_order),
    CONSTRAINT fk_routing_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_routing_category FOREIGN KEY (category_id) REFERENCES categories (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE document_categories (
    id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id  CHAR(26)        NOT NULL,
    tenant_id  BIGINT UNSIGNED NOT NULL,
    cat_key    VARCHAR(64)     NOT NULL,
    name       VARCHAR(191)    NOT NULL,
    is_active  TINYINT(1)      NOT NULL DEFAULT 1,
    created_at DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_doc_categories_public_id (public_id),
    UNIQUE KEY uq_doc_categories_key (tenant_id, cat_key),
    CONSTRAINT fk_doc_categories_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
