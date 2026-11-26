-- name: DeleteSessionPushDeviceExcept :exec
-- One device per session: a session that registers another token drops its
-- previous one.
DELETE FROM push_devices WHERE session_id = $1 AND NOT (provider = $2 AND push_token = $3);

-- name: UpsertPushDevice :one
-- Registers the device of a session. The same token registered before (by
-- this or another session, also of another user: the phone changed hands)
-- moves to this session.
INSERT INTO push_devices (user_id, session_id, provider, push_token, account_ids)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (provider, push_token) DO UPDATE SET
  user_id = EXCLUDED.user_id, session_id = EXCLUDED.session_id, account_ids = EXCLUDED.account_ids,
  last_used_at = now(), last_error = NULL
RETURNING *;

-- name: GetSessionPushDevice :one
SELECT * FROM push_devices WHERE session_id = $1;

-- name: DeletePushDevice :execrows
DELETE FROM push_devices WHERE id = $1 AND user_id = $2;

-- name: CountUserAccountsIn :one
SELECT count(*) FROM accounts WHERE user_id = $1 AND id = ANY(sqlc.arg(ids)::bigint[]);

-- name: ListPushTargets :many
-- Devices that get the push of new mail of an account: live session, user
-- not disabled, the account among account_ids (NULL: all). language is
-- users.settings.language ('' when unset).
SELECT d.id, d.provider, d.push_token, COALESCE(u.settings->>'language', '')::text AS language
FROM push_devices d
JOIN accounts a ON a.user_id = d.user_id
JOIN sessions s ON s.id = d.session_id
JOIN users u ON u.id = d.user_id
WHERE a.id = sqlc.arg(account_id) AND s.expires_at > now() AND u.disabled_at IS NULL
  AND (d.account_ids IS NULL OR sqlc.arg(account_id)::bigint = ANY(d.account_ids))
ORDER BY d.id;

-- name: ListPushMessages :many
-- The messages of a push.notify job that still deserve it: in INBOX (as the
-- user sees it), unread, not from one of the user's own addresses. Newest
-- first.
SELECT m.id, m.thread_id FROM messages m
WHERE m.account_id = sqlc.arg(account_id) AND m.id = ANY(sqlc.arg(ids)::bigint[])
  AND m.orphaned_at IS NULL AND m.is_unread
  AND EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
              WHERE ml.message_id = m.id AND l.account_id = m.account_id AND l.system_key = 'INBOX')
  AND NOT EXISTS (SELECT 1 FROM accounts o WHERE o.user_id = m.user_id AND o.email = m.from_addr)
ORDER BY m.internal_date DESC, m.id DESC;

-- name: PushDeviceSent :exec
UPDATE push_devices SET last_used_at = now(), last_error = NULL WHERE id = $1;

-- name: PushDeviceFailed :exec
UPDATE push_devices SET last_error = $2 WHERE id = $1;

-- name: DeletePushDeviceByID :exec
DELETE FROM push_devices WHERE id = $1;
