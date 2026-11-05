-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: InsertUser :one
INSERT INTO users (email, display_name, password_hash, is_admin)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: UpdateUserPasswordHash :exec
UPDATE users SET password_hash = $2 WHERE id = $1;

-- name: ListUsers :many
SELECT * FROM users ORDER BY id;

-- name: LockUser :one
SELECT * FROM users WHERE id = $1 FOR UPDATE;

-- name: SetUserPassword :exec
-- Silo password of mode local (also stamps the change).
UPDATE users SET password_hash = $2, password_changed_at = now() WHERE id = $1;

-- name: UpdateUserAdmin :one
-- Admin panel: nil arguments keep the column.
UPDATE users SET
  display_name = COALESCE(sqlc.narg('display_name'), display_name),
  is_admin     = COALESCE(sqlc.narg('is_admin'), is_admin),
  disabled_at  = CASE
                   WHEN sqlc.narg('disabled')::bool IS NULL THEN disabled_at
                   WHEN sqlc.narg('disabled')::bool THEN COALESCE(disabled_at, now())
                   ELSE NULL
                 END
WHERE id = @id
RETURNING *;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1;

-- name: SetTOTPSecret :exec
-- Starts (or restarts) TOTP setup: the new secret is not enforced until enabled.
UPDATE users SET totp_secret_enc = $2, totp_enabled_at = NULL, totp_last_step = 0, totp_backup_codes = '{}'
WHERE id = $1;

-- name: EnableTOTP :exec
UPDATE users SET totp_enabled_at = now(), totp_last_step = $2, totp_backup_codes = $3 WHERE id = $1;

-- name: SetTOTPLastStep :exec
UPDATE users SET totp_last_step = $2 WHERE id = $1;

-- name: SetTOTPBackupCodes :exec
UPDATE users SET totp_backup_codes = $2 WHERE id = $1;

-- name: UseTOTPBackupCode :execrows
-- Consumes one backup code (by hash); 0 rows when it is not (or no longer) valid.
UPDATE users SET totp_backup_codes = array_remove(totp_backup_codes, @code_hash::text)
WHERE id = @id AND @code_hash::text = ANY (totp_backup_codes);

-- name: ClearTOTP :exec
UPDATE users SET totp_secret_enc = NULL, totp_enabled_at = NULL, totp_last_step = 0, totp_backup_codes = '{}'
WHERE id = $1;

-- name: ListUserTOTPSecrets :many
-- Key rotation: every sealed TOTP secret.
SELECT id FROM users WHERE totp_secret_enc IS NOT NULL ORDER BY id;

-- name: LockUserTOTPSecret :one
SELECT totp_secret_enc FROM users WHERE id = $1 FOR UPDATE;

-- name: UpdateUserTOTPSecret :exec
UPDATE users SET totp_secret_enc = $2 WHERE id = $1;
