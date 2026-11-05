-- 000050: local sign-in mode (plan 3.15, phase 5): account lockout, password
-- change tracking and TOTP second factor on users.
--
-- Numbered 000050 to stay clear of the migrations of phase 4, developed in
-- parallel (golang-migrate accepts gaps).

ALTER TABLE users
  -- AES-256-GCM (secrets.Keyring, AAD = users.id) of the raw 20-byte TOTP
  -- secret. Set with totp_enabled_at NULL: setup in progress, not enforced.
  ADD COLUMN totp_secret_enc     bytea       NULL,
  ADD COLUMN totp_enabled_at     timestamptz NULL,
  -- Last accepted TOTP time step: a code of this step or an earlier one is
  -- refused (replay protection).
  ADD COLUMN totp_last_step      bigint      NOT NULL DEFAULT 0,
  -- sha256 hex of the unused backup codes (single use).
  ADD COLUMN totp_backup_codes   text[]      NOT NULL DEFAULT '{}',
  -- A disabled user cannot sign in and their sessions stop working.
  ADD COLUMN disabled_at         timestamptz NULL,
  ADD COLUMN password_changed_at timestamptz NULL;
