-- 000054: devices that show new-mail notifications themselves (ADR 0016,
-- update 2026-11-26). data_only = true: the push carries no title (an FCM
-- data message that wakes the app), and the app fetches sender, subject and
-- snippet from this server (GET /messages) and presents the notification.
-- Devices registered before (or by apps that do not ask for it) keep the
-- titled push the system shows.

ALTER TABLE push_devices ADD COLUMN data_only boolean NOT NULL DEFAULT false;
