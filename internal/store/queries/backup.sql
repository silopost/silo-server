-- name: InsertMetadataRestore :exec
-- Stages a user-label assignment or a snooze until its messages are synced
-- (import-metadata, rebuild-cache).
INSERT INTO metadata_restores (user_id, account_id, kind, payload)
VALUES ($1, $2, $3, $4);

-- name: ListMetadataRestores :many
SELECT * FROM metadata_restores
WHERE account_id = $1 AND user_id = $2
ORDER BY id;

-- name: UpdateMetadataRestore :exec
UPDATE metadata_restores SET payload = $3, attempts = attempts + 1, last_error = $4
WHERE id = $1 AND user_id = $2;

-- name: DeleteMetadataRestore :exec
DELETE FROM metadata_restores WHERE id = $1 AND user_id = $2;
