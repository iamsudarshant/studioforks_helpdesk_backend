DROP INDEX ix_categories_department ON categories;

-- The column returns to being unpopulated; it is not dropped, because it was
-- part of the schema before this migration ran.
UPDATE categories SET default_department_id = NULL;
