-- name: ListAccountsByUser :many
SELECT * FROM accounts
WHERE user_id = $1
ORDER BY position, id;

-- name: GetAccount :one
SELECT * FROM accounts
WHERE id = $1 AND user_id = $2;

-- name: CountAccountsByUser :one
SELECT count(*) FROM accounts WHERE user_id = $1;

-- name: InsertAccount :one
INSERT INTO accounts (
  user_id, email, display_name, provider,
  imap_host, imap_port, imap_tls, smtp_host, smtp_port, smtp_tls,
  username, auth_type, secret_enc, position, sent_append
) VALUES (
  $1, $2, $3, $4,
  $5, $6, $7, $8, $9, $10,
  $11, $12, $13, $14,
  -- plan 3.9: generic servers need Silo to save sent mail in Sent; Gmail
  -- and Office 365 save it themselves.
  $4::text = 'generic'
)
RETURNING *;

-- name: ListAccountIDs :many
-- tenancy: exempt key rotation touches every tenant
SELECT id FROM accounts ORDER BY id;

-- name: LockAccountSecret :one
-- tenancy: exempt key rotation touches every tenant
SELECT secret_enc FROM accounts WHERE id = $1 FOR UPDATE;

-- name: UpdateAccountSecret :exec
-- tenancy: exempt key rotation touches every tenant
UPDATE accounts SET secret_enc = $2 WHERE id = $1;

-- name: GetAccountByLogin :one
SELECT * FROM accounts
WHERE user_id = $1 AND imap_host = $2 AND username = $3;

-- name: SetAccountSyncState :exec
UPDATE accounts SET sync_state = $3, sync_error = $4
WHERE id = $1 AND user_id = $2;

-- name: NextAccountPosition :one
SELECT (COALESCE(MAX(position) + 1, 0))::int FROM accounts WHERE user_id = $1;

-- name: UpdateAccount :one
UPDATE accounts SET
  display_name = COALESCE(sqlc.narg('display_name'), display_name),
  imap_port    = COALESCE(sqlc.narg('imap_port'), imap_port),
  imap_tls     = COALESCE(sqlc.narg('imap_tls'), imap_tls),
  smtp_port    = COALESCE(sqlc.narg('smtp_port'), smtp_port),
  smtp_tls     = COALESCE(sqlc.narg('smtp_tls'), smtp_tls),
  sent_append  = COALESCE(sqlc.narg('sent_append'), sent_append)
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: DeleteAccount :execrows
DELETE FROM accounts WHERE id = $1 AND user_id = $2;
