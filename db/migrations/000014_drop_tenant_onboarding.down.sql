-- 000014 rollback: recreate the onboarding wizard table.
-- Note: any previously stored wizard state was dropped, so rows are empty.

CREATE TABLE tenant_onboarding (
    tenant_id            BIGINT UNSIGNED NOT NULL,
    current_step         VARCHAR(64)     NOT NULL,
    payload_json         JSON                NULL,
    completed_steps_json JSON                NULL,
    completed_at         DATETIME(3)         NULL,
    updated_by           BIGINT UNSIGNED     NULL,
    updated_at           DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    PRIMARY KEY (tenant_id),
    CONSTRAINT fk_tenant_onboarding_tenant FOREIGN KEY (tenant_id) REFERENCES tenants (id) ON DELETE CASCADE,
    CONSTRAINT fk_tenant_onboarding_updater FOREIGN KEY (updated_by) REFERENCES users (id) ON DELETE SET NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
