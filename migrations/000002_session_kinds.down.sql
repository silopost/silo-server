-- Reverts 000002_session_kinds. Bearer sessions would turn into cookie
-- sessions, so they are deleted first.
DELETE FROM sessions WHERE kind = 'bearer';

ALTER TABLE sessions
  DROP COLUMN client_name,
  DROP COLUMN kind;
