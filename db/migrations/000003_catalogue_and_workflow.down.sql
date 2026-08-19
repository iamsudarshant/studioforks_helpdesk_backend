ALTER TABLE user_groups DROP FOREIGN KEY fk_user_groups_sla;
ALTER TABLE sla_policies DROP FOREIGN KEY fk_sla_category;
DROP TABLE IF EXISTS document_categories;
DROP TABLE IF EXISTS routing_rules;
DROP TABLE IF EXISTS category_workflows;
DROP TABLE IF EXISTS category_fields;
DROP TABLE IF EXISTS categories;
DROP TABLE IF EXISTS sla_policies;
DROP TABLE IF EXISTS business_hours;
