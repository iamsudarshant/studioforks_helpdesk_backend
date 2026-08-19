-- ---------------------------------------------------------------------------
-- 000025 Files chosen in a category's own form become attachments
--
-- A category can declare a FILE field, and every PF and ESI category ships with
-- one called "Supporting document". The browser uploads it to /documents like
-- any other file and stores the resulting id as the field's value inside
-- `tickets.custom_fields_json`.
--
-- Nothing ever wrote the matching `ticket_attachments` row. The document
-- existed, was stored on disk, counted against quota and was authorised through
-- the tickets it was attached to — of which there were none. So the file was
-- unreachable: the ticket displayed a 26-character identifier where a filename
-- should have been, the Attachments tab was empty, and the person the document
-- had been sent to could not open it.
--
-- The dropzone at the foot of the same form did write the row, which is why one
-- half of the create screen worked and the other did not. The application now
-- links both (see internal/ticket/repository_form_files.go); this backfills the
-- tickets raised before it did.
--
-- Scope and safety:
--
--   * only FILE fields are read, so a text field that happens to hold a
--     26-character string is never mistaken for a document reference;
--   * `d.tenant_id = t.tenant_id` is the isolation guarantee — a reference
--     copied between clients resolves to nothing rather than to somebody
--     else's file;
--   * the NOT EXISTS guard makes this idempotent, so re-running it changes
--     nothing and a ticket that already had the attachment keeps one row;
--   * `uploaded_by` comes from the document rather than from the ticket's
--     creator, because the document knows who actually sent it.
--
-- JSON_SEARCH rather than JSON_TABLE, which MariaDB 10.4 does not have: it
-- matches the whole string anywhere in the field's value, so it reads a lone
-- reference and a list of them the same way, without unrolling the array.
-- ---------------------------------------------------------------------------

INSERT INTO ticket_attachments (tenant_id, ticket_id, document_id, context, uploaded_by)
SELECT DISTINCT
       t.tenant_id,
       t.id,
       d.id,
       'REQUESTER',
       d.uploaded_by
FROM tickets t
JOIN categories c
  ON c.id = t.category_id
-- The field set a ticket's category actually presents, which for a subcategory
-- is its parent's: a subcategory carries no fields of its own.
JOIN category_fields f
  ON f.tenant_id = t.tenant_id
 AND f.field_type = 'FILE'
 AND (f.category_id = t.category_id OR f.category_id = c.parent_id)
JOIN documents d
  ON d.tenant_id = t.tenant_id
 AND d.deleted_at IS NULL
 AND JSON_SEARCH(
       JSON_EXTRACT(t.custom_fields_json, CONCAT('$."', f.field_key, '"')),
       'one',
       d.public_id
     ) IS NOT NULL
WHERE t.deleted_at IS NULL
  AND t.custom_fields_json IS NOT NULL
  AND JSON_VALID(t.custom_fields_json)
  AND NOT EXISTS (
        SELECT 1 FROM ticket_attachments ta
        WHERE ta.ticket_id = t.id AND ta.document_id = d.id
      );
