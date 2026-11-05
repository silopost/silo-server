DROP INDEX IF EXISTS messages_body_pending;
DROP INDEX IF EXISTS messages_search_stale;
ALTER TABLE messages DROP COLUMN IF EXISTS search_ver;
