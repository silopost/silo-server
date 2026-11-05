-- name: EnqueueJob :one
INSERT INTO jobs (kind, account_id, group_id, payload, run_at)
VALUES ($1, $2, $3, $4, COALESCE(sqlc.narg('run_at')::timestamptz, now()))
RETURNING *;

-- name: ClaimGeneralJob :one
-- Takes the next due job that belongs to no account (plan 3.13). A job whose
-- lock expired (crashed worker) is picked up again.
UPDATE jobs SET locked_until = now() + interval '5 minutes', attempts = attempts + 1
WHERE id = (SELECT j.id FROM jobs j
            WHERE j.account_id IS NULL AND j.status = 'pending' AND j.run_at <= now()
              AND (j.locked_until IS NULL OR j.locked_until < now())
            ORDER BY j.run_at, j.id FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;

-- name: ClaimAccountJob :one
-- Same as ClaimGeneralJob for one account, strictly ordered by (run_at, id).
UPDATE jobs SET locked_until = now() + interval '5 minutes', attempts = attempts + 1
WHERE id = (SELECT j.id FROM jobs j
            WHERE j.account_id = $1 AND j.status = 'pending' AND j.run_at <= now()
              AND (j.locked_until IS NULL OR j.locked_until < now())
            ORDER BY j.run_at, j.id FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;

-- name: CompleteJob :exec
DELETE FROM jobs WHERE id = $1;

-- name: RetryJob :exec
UPDATE jobs SET run_at = $2, locked_until = NULL, last_error = $3 WHERE id = $1;

-- name: BuryJob :exec
UPDATE jobs SET status = 'dead', locked_until = NULL, last_error = $2 WHERE id = $1;

-- name: ReleaseJob :exec
-- Gives a claimed job back without counting the attempt (shutdown interrupted it).
UPDATE jobs SET locked_until = NULL, attempts = attempts - 1 WHERE id = $1;

-- name: DeferJob :exec
-- Reschedules a claimed job without counting the attempt (the handler asked
-- to wait, e.g. its account is offline).
UPDATE jobs SET run_at = $2, locked_until = NULL, attempts = attempts - 1, last_error = $3 WHERE id = $1;
