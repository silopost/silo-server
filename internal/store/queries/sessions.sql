-- name: InsertSession :one
INSERT INTO sessions (user_id, token_hash, csrf_token, expires_at, user_agent, ip)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSessionByTokenHash :one
-- tenancy: exempt the session is how the user is identified in the first place
SELECT * FROM sessions WHERE token_hash = $1;

-- name: TouchSession :exec
-- Throttled: a session seen within the last minute is not rewritten.
UPDATE sessions SET last_seen_at = now(), expires_at = $3
WHERE id = $1 AND user_id = $2 AND last_seen_at < now() - interval '1 minute';

-- name: ListSessionsByUser :many
SELECT * FROM sessions WHERE user_id = $1 AND expires_at > now()
ORDER BY last_seen_at DESC, id DESC;

-- name: DeleteSession :execrows
DELETE FROM sessions WHERE id = $1 AND user_id = $2;

-- name: DeleteExpiredSessions :exec
-- tenancy: exempt housekeeping across all users
DELETE FROM sessions WHERE expires_at <= now();
