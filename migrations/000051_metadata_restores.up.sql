-- 000051: staging for metadata that points at messages not mirrored yet
-- (plan 3.3, phase 5: import-metadata and rebuild-cache).
--
-- User-label assignments and snoozes are keyed by message_id_hdr in a backup
-- or a rebuild; right after an import or a rebuild the messages are not in
-- the cache yet. Each row waits here until the metadata_restore job (package
-- internal/backup) finds the messages after the sync ingested them. payload:
--   label:  {"label": "<name>", "messageIdHdrs": ["...", ...]}
--   snooze: {"wakeAt": "...", "createdAt": "...", "items": ["...", ...],
--            "sourceFolders": {"<message_id_hdr>": "<folder name>"}}
-- Matched ids leave the payload; a row is deleted once fully applied. Rows the
-- job gives up on (24 h) stay with last_error for the admin.
--
-- Numbered 000051 to stay clear of the migrations of phase 4 (000006..).

CREATE TABLE metadata_restores (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  kind       text   NOT NULL CHECK (kind IN ('label','snooze')),
  payload    jsonb  NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  attempts   int    NOT NULL DEFAULT 0,
  last_error text   NULL
);
CREATE INDEX metadata_restores_account ON metadata_restores (account_id, id);
