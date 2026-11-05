ALTER TABLE users
  DROP COLUMN password_changed_at,
  DROP COLUMN disabled_at,
  DROP COLUMN totp_backup_codes,
  DROP COLUMN totp_last_step,
  DROP COLUMN totp_enabled_at,
  DROP COLUMN totp_secret_enc;
