ALTER TABLE sites
    DROP INDEX ix_sites_tenant_active,
    DROP COLUMN is_default,
    DROP COLUMN pincode,
    DROP COLUMN address;

DROP TABLE IF EXISTS entity_registrations;

ALTER TABLE entities
    DROP INDEX ix_entities_template,
    DROP COLUMN gst_number,
    DROP COLUMN cin_number,
    DROP COLUMN registered_address,
    DROP COLUMN opted_out_by,
    DROP COLUMN opted_out_at,
    DROP COLUMN is_default,
    DROP COLUMN template_key;

DROP TABLE IF EXISTS entity_templates;

ALTER TABLE tenants
    DROP INDEX uq_tenants_client_code,
    DROP COLUMN account_manager_id,
    DROP COLUMN industry,
    DROP COLUMN alt_phone,
    DROP COLUMN alt_email,
    DROP COLUMN client_code;
