-- ---------------------------------------------------------------------------
-- 000005 Document storage, encryption keys, versions, access log
--
-- Files live on disk outside the web root; stored_path is always relative to
-- the storage root and is never exposed through the API.
-- ---------------------------------------------------------------------------

CREATE TABLE encryption_keys (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    key_id      CHAR(26)        NOT NULL,
    -- Data encryption key, itself sealed with the master KEK from env/KMS.
    wrapped_dek VARBINARY(512)  NOT NULL,
    algo        VARCHAR(32)     NOT NULL DEFAULT 'AES-256-GCM',
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    rotated_at  DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_encryption_keys_key_id (key_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE documents (
    id                  BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id           CHAR(26)        NOT NULL,
    tenant_id           BIGINT UNSIGNED NOT NULL,
    original_name       VARCHAR(255)    NOT NULL,
    stored_path         VARCHAR(512)    NOT NULL,
    mime_type           VARCHAR(128)    NOT NULL,
    size_bytes          BIGINT UNSIGNED NOT NULL DEFAULT 0,
    checksum_sha256     CHAR(64)            NULL,
    document_category_id BIGINT UNSIGNED    NULL,
    description         VARCHAR(500)        NULL,
    version             INT             NOT NULL DEFAULT 1,
    current_version_id  BIGINT UNSIGNED     NULL,
    is_encrypted        TINYINT(1)      NOT NULL DEFAULT 1,
    encryption_key_id   CHAR(26)            NULL,
    nonce               VARBINARY(64)       NULL,
    scan_status         VARCHAR(16)     NOT NULL DEFAULT 'SKIPPED',
    owner_type          VARCHAR(16)     NOT NULL DEFAULT 'TICKET',
    owner_id            BIGINT UNSIGNED     NULL,
    uploaded_by         BIGINT UNSIGNED     NULL,
    retention_until     DATETIME(3)         NULL,
    created_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    updated_at          DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    deleted_at          DATETIME(3)         NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uq_documents_public_id (public_id),
    KEY ix_documents_owner (tenant_id, owner_type, owner_id),
    KEY ix_documents_tenant_created (tenant_id, created_at),
    KEY ix_documents_retention (retention_until),
    KEY ix_documents_checksum (tenant_id, checksum_sha256),
    CONSTRAINT fk_documents_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_documents_category FOREIGN KEY (document_category_id) REFERENCES document_categories (id) ON DELETE SET NULL,
    CONSTRAINT fk_documents_uploader FOREIGN KEY (uploaded_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE document_versions (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    public_id       CHAR(26)        NOT NULL,
    tenant_id       BIGINT UNSIGNED NOT NULL,
    document_id     BIGINT UNSIGNED NOT NULL,
    version         INT             NOT NULL,
    stored_path     VARCHAR(512)    NOT NULL,
    original_name   VARCHAR(255)    NOT NULL,
    mime_type       VARCHAR(128)    NOT NULL,
    size_bytes      BIGINT UNSIGNED NOT NULL DEFAULT 0,
    checksum_sha256 CHAR(64)            NULL,
    encryption_key_id CHAR(26)          NULL,
    nonce           VARBINARY(64)       NULL,
    change_note     VARCHAR(500)        NULL,
    uploaded_by     BIGINT UNSIGNED     NULL,
    created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_document_versions_public_id (public_id),
    UNIQUE KEY uq_document_version (document_id, version),
    KEY ix_document_versions_tenant (tenant_id, document_id),
    CONSTRAINT fk_document_versions_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_document_versions_document FOREIGN KEY (document_id) REFERENCES documents (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Derived artefacts (Office -> PDF preview, image/PDF thumbnails) cached by the
-- worker so a repeat preview does not re-run LibreOffice.
CREATE TABLE document_previews (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    document_id BIGINT UNSIGNED NOT NULL,
    kind        VARCHAR(16)     NOT NULL,
    stored_path VARCHAR(512)    NOT NULL,
    mime_type   VARCHAR(128)    NOT NULL,
    size_bytes  BIGINT UNSIGNED NOT NULL DEFAULT 0,
    nonce       VARBINARY(64)       NULL,
    encryption_key_id CHAR(26)      NULL,
    status      VARCHAR(16)     NOT NULL DEFAULT 'READY',
    error       VARCHAR(500)        NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_document_previews (document_id, kind),
    KEY ix_document_previews_tenant (tenant_id),
    CONSTRAINT fk_document_previews_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_document_previews_document FOREIGN KEY (document_id) REFERENCES documents (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

CREATE TABLE ticket_attachments (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id       BIGINT UNSIGNED NOT NULL,
    ticket_id       BIGINT UNSIGNED NOT NULL,
    conversation_id BIGINT UNSIGNED     NULL,
    document_id     BIGINT UNSIGNED NOT NULL,
    context         VARCHAR(16)     NOT NULL DEFAULT 'REQUESTER',
    uploaded_by     BIGINT UNSIGNED     NULL,
    created_at      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    UNIQUE KEY uq_ticket_attachment (ticket_id, document_id),
    KEY ix_attachments_ticket (tenant_id, ticket_id),
    KEY ix_attachments_conversation (conversation_id),
    CONSTRAINT fk_attachments_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_attachments_ticket FOREIGN KEY (ticket_id) REFERENCES tickets (id) ON DELETE CASCADE,
    CONSTRAINT fk_attachments_document FOREIGN KEY (document_id) REFERENCES documents (id) ON DELETE CASCADE,
    CONSTRAINT fk_attachments_conversation FOREIGN KEY (conversation_id) REFERENCES ticket_conversations (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;

-- Feeds both compliance reporting and the "downloaded by X" timeline entry.
CREATE TABLE document_access_log (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    tenant_id   BIGINT UNSIGNED NOT NULL,
    document_id BIGINT UNSIGNED NOT NULL,
    user_id     BIGINT UNSIGNED     NULL,
    action      VARCHAR(16)     NOT NULL,
    ip          VARCHAR(45)         NULL,
    user_agent  VARCHAR(255)        NULL,
    created_at  DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (id),
    KEY ix_doc_access_document (tenant_id, document_id, created_at),
    KEY ix_doc_access_user (tenant_id, user_id, created_at),
    CONSTRAINT fk_doc_access_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_doc_access_document FOREIGN KEY (document_id) REFERENCES documents (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
