-- Admin panel (plan 5.1, phase 5): cross-tenant reads and job maintenance.
-- Only reachable by users with users.is_admin.

-- name: AdminListAccounts :many
-- tenancy: exempt admin panel lists the accounts of every user
SELECT a.id, a.user_id, u.email AS user_email, a.email, a.display_name, a.imap_host,
       a.sync_state, a.sync_error, a.last_sync_at, a.created_at,
       (SELECT count(*) FROM jobs j WHERE j.account_id = a.id AND j.status = 'pending' AND j.kind = 'imap_op')::int AS pending_ops,
       (SELECT count(*) FROM jobs j WHERE j.account_id = a.id AND j.status = 'dead')::int AS dead_ops
FROM accounts a JOIN users u ON u.id = a.user_id
ORDER BY a.user_id, a.position, a.id;

-- name: AdminListUserAccounts :many
-- tenancy: exempt admin panel lists the accounts of every user
SELECT * FROM accounts ORDER BY user_id, position, id;

-- name: AdminListJobs :many
-- tenancy: exempt admin panel lists jobs of every account
SELECT j.id, j.kind, j.account_id, a.user_id AS account_user_id, j.attempts, j.max_attempts, j.status,
       j.last_error, j.run_at, j.created_at
FROM jobs j LEFT JOIN accounts a ON a.id = j.account_id
WHERE (sqlc.narg('status')::text IS NULL OR j.status = sqlc.narg('status')::text)
  AND (sqlc.narg('account_id')::bigint IS NULL OR j.account_id = sqlc.narg('account_id')::bigint)
ORDER BY j.id DESC
LIMIT 200;

-- name: AdminRetryJob :one
-- A dead (or waiting) job runs again as soon as a worker is free.
-- tenancy: exempt admin panel retries jobs of every account
WITH j AS (
  UPDATE jobs SET status = 'pending', attempts = 0, run_at = now(), locked_until = NULL
  WHERE jobs.id = $1 AND jobs.status IN ('pending', 'dead')
    AND (jobs.locked_until IS NULL OR jobs.locked_until < now())
  RETURNING *
)
SELECT j.id, j.kind, j.account_id, a.user_id AS account_user_id, j.attempts, j.max_attempts, j.status,
       j.last_error, j.run_at, j.created_at
FROM j LEFT JOIN accounts a ON a.id = j.account_id;

-- name: AdminDeleteJob :execrows
DELETE FROM jobs WHERE id = $1 AND (status = 'dead' OR locked_until IS NULL OR locked_until < now());

-- name: AdminSetUserAccountsDisabled :many
-- tenancy: exempt account states follow the user being disabled by an admin; user_id is the filter
UPDATE accounts SET sync_state = 'disabled', sync_error = NULL
WHERE user_id = $1 AND sync_state <> 'disabled'
RETURNING id;

-- name: AdminReenableUserAccounts :many
UPDATE accounts SET sync_state = 'connecting', sync_error = NULL
WHERE user_id = $1 AND sync_state = 'disabled'
RETURNING id;
