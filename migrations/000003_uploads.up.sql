-- 000003_uploads: draft attachments uploaded before they belong to a message
-- (plan 5.6). An upload is an attachments row with message_id NULL whose blob
-- (kind 'local') is referenced once; PUT /drafts/{id} pins it to the draft's
-- message. created_at is when the row was created or last unpinned: blob_gc
-- deletes unpinned rows older than 24 h and releases their blob.

ALTER TABLE attachments
  ALTER COLUMN message_id DROP NOT NULL,
  ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX attachments_unpinned ON attachments (created_at) WHERE message_id IS NULL;
