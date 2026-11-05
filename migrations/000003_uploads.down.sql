-- Reverts 000003_uploads. Unpinned uploads cannot exist without a message:
-- their blob references are released and the rows deleted.
UPDATE blobs b SET ref_count = GREATEST(b.ref_count - c.n, 0)
FROM (SELECT blob_sha256 AS sha, count(*)::int AS n FROM attachments
      WHERE message_id IS NULL AND blob_sha256 IS NOT NULL GROUP BY blob_sha256) c
WHERE b.sha256 = c.sha;
DELETE FROM attachments WHERE message_id IS NULL;

DROP INDEX attachments_unpinned;
ALTER TABLE attachments
  DROP COLUMN created_at,
  ALTER COLUMN message_id SET NOT NULL;
