-- 000013 rollback: drop Help module and revert org model.

DROP TABLE IF EXISTS help_ticket_replies;
DROP TABLE IF EXISTS help_tickets;
DROP TABLE IF EXISTS faq_articles;

ALTER TABLE entities
    DROP FOREIGN KEY fk_entities_department,
    DROP KEY ix_entities_department,
    DROP COLUMN department_id;

ALTER TABLE departments
    DROP COLUMN type;
