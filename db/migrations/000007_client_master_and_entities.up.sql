-- ---------------------------------------------------------------------------
-- 000007 Client master, entity registrations and default entity templates
--
-- A "client" is the workspace: Ampersand Group IS the tenant row. This migration
-- completes the client master record, and redefines an entity as a legally
-- registered establishment of that client — each carrying its own PF and/or
-- ESIC registration — so a ticket form can offer only the entities registered
-- for the scheme being queried.
-- ---------------------------------------------------------------------------

-- --- client master ---------------------------------------------------------

ALTER TABLE tenants
    ADD COLUMN client_code  VARCHAR(32)  NULL AFTER slug,
    ADD COLUMN alt_email    VARCHAR(191) NULL AFTER contact_email,
    ADD COLUMN alt_phone    VARCHAR(32)  NULL AFTER contact_phone,
    -- tax_id already holds the GST number; named generically because not every
    -- jurisdiction calls it GST.
    ADD COLUMN industry     VARCHAR(96)  NULL AFTER legal_name,
    ADD COLUMN account_manager_id BIGINT UNSIGNED NULL AFTER created_by;

-- Client code is the human-facing identifier operators quote on the phone, so
-- it must be unique but may be absent until onboarding fills it in.
ALTER TABLE tenants
    ADD UNIQUE KEY uq_tenants_client_code (client_code);

-- --- default entity catalogue ----------------------------------------------

-- Platform-level templates applied when a client is created. A client keeps the
-- ones that apply and opts out of the rest, which is why every client ends up
-- with a different entity set.
CREATE TABLE entity_templates (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id    CHAR(26)        NOT NULL,
    template_key VARCHAR(64)     NOT NULL,
    name         VARCHAR(191)    NOT NULL,
    description  VARCHAR(500)        NULL,
    entity_type  VARCHAR(48)         NULL,
    -- Category keys this template is normally registered for, e.g.
    -- ["PF_QUERY","ESI_QUERY"]. Applied as entity_registrations rows.
    default_categories_json LONGTEXT NULL CHECK (default_categories_json IS NULL OR JSON_VALID(default_categories_json)),
    is_active    TINYINT(1)      NOT NULL DEFAULT 1,
    sort_order   INT             NOT NULL DEFAULT 0,
    created_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at   DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_entity_templates_public_id (public_id),
    UNIQUE KEY uq_entity_templates_key (template_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- --- entities as registered establishments ---------------------------------

ALTER TABLE entities
    -- Seeded from a template, so the UI can show "default" versus "added by you"
    -- and a client can opt out without the entity being recreated next time.
    ADD COLUMN template_key       VARCHAR(64)  NULL AFTER type,
    ADD COLUMN is_default         TINYINT(1)   NOT NULL DEFAULT 0 AFTER template_key,
    ADD COLUMN opted_out_at       DATETIME(3)  NULL AFTER is_active,
    ADD COLUMN opted_out_by       BIGINT UNSIGNED NULL AFTER opted_out_at,
    ADD COLUMN registered_address TEXT         NULL AFTER address,
    ADD COLUMN cin_number         VARCHAR(32)  NULL,
    ADD COLUMN gst_number         VARCHAR(32)  NULL;

ALTER TABLE entities
    ADD KEY ix_entities_template (tenant_id, template_key);

-- One row per (entity, category) the entity is registered for. The PF category
-- carries the EPFO establishment code, ESI the ESIC code; a category with no
-- statutory number (IT, HR) simply has a NULL registration_number.
--
-- This is what makes point 4 work: choosing PF on the ticket form lists only the
-- entities with an active PF registration.
CREATE TABLE entity_registrations (
    id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id           CHAR(26)        NOT NULL,
    tenant_id           BIGINT UNSIGNED NOT NULL,
    entity_id           BIGINT UNSIGNED NOT NULL,
    category_id         BIGINT UNSIGNED NOT NULL,
    registration_number VARCHAR(64)         NULL,
    registered_on       DATE                NULL,
    valid_until         DATE                NULL,
    notes               VARCHAR(500)        NULL,
    is_active           TINYINT(1)      NOT NULL DEFAULT 1,
    created_by          BIGINT UNSIGNED     NULL,
    created_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_entity_registrations_public_id (public_id),
    UNIQUE KEY uq_entity_registration (entity_id, category_id),
    -- The ticket form's lookup: tenant + category -> active entities.
    KEY ix_entity_registrations_lookup (tenant_id, category_id, is_active),
    KEY ix_entity_registrations_entity (entity_id, is_active),
    CONSTRAINT fk_entity_registrations_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_entity_registrations_entity FOREIGN KEY (entity_id) REFERENCES entities (id) ON DELETE CASCADE,
    CONSTRAINT fk_entity_registrations_category FOREIGN KEY (category_id) REFERENCES categories (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- --- sites belong to the client, not to an entity --------------------------

-- A client may have locations with no entity mapped at all ("It is possible
-- there is no location mapped to client as well"), so entity_id stays nullable
-- and sites are addressed from the client downwards.
ALTER TABLE sites
    ADD COLUMN address    TEXT        NULL AFTER name,
    ADD COLUMN pincode    VARCHAR(12) NULL AFTER state,
    ADD COLUMN is_default TINYINT(1)  NOT NULL DEFAULT 0 AFTER is_active;

-- Employees are located at a site; the ticket already snapshots it.
ALTER TABLE sites
    ADD KEY ix_sites_tenant_active (tenant_id, is_active);
