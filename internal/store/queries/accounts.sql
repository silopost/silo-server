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
  username, auth_type, secret_enc, position
) VALUES (
  $1, $2, $3, $4,
  $5, $6, $7, $8, $9, $10,
  $11, $12, $13, $14
)
RETURNING *;
