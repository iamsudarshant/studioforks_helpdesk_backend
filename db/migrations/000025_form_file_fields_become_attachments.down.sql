-- ---------------------------------------------------------------------------
-- 000025 down
--
-- Deliberately a no-op.
--
-- The up migration links documents the requester had already sent to the ticket
-- they sent them with. Reversing it would mean deciding which of a ticket's
-- attachments were linked by the backfill and which were linked by the create
-- path — a distinction the rows do not record, because they are identical by
-- construction and should be.
--
-- Removing them all would detach files uploaded through the dropzone as well,
-- which is data loss for a rollback that was only ever meant to undo a schema
-- change. There is no schema change here to undo: nothing was added, altered or
-- dropped, and leaving the rows in place is correct on either side of a
-- rollback. An operator who genuinely wants them gone can delete by
-- `ticket_attachments.created_at` against the deployment window.
-- ---------------------------------------------------------------------------

SELECT 1;
