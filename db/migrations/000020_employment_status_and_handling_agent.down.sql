DROP INDEX ix_users_handling_agent ON users;

ALTER TABLE users
    DROP FOREIGN KEY fk_users_handling_agent;

ALTER TABLE users
    DROP COLUMN employment_changed_by,
    DROP COLUMN employment_changed_at,
    DROP COLUMN handling_agent_id;
