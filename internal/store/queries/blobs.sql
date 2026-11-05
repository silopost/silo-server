-- name: UpsertBlobRef :exec
-- Registers one more reference to a blob, creating the row on first use.
INSERT INTO blobs (sha256, size_bytes, kind, ref_count)
VALUES ($1, $2, $3, 1)
ON CONFLICT (sha256) DO UPDATE SET ref_count = blobs.ref_count + 1;

-- name: ReleaseBlobRef :exec
UPDATE blobs SET ref_count = ref_count - 1
WHERE sha256 = $1 AND ref_count > 0;

-- name: ListUnreferencedBlobs :many
SELECT sha256, size_bytes, kind FROM blobs
WHERE ref_count = 0 AND created_at < $1
ORDER BY created_at
LIMIT $2;
