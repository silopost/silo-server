-- 000009_label_folders_rebuild: per-folder keyword state of user labels and
-- the cache rebuild of an account (plan 3.3, 3.7, 5.1).
--
-- label_folder_state: a user label whose keyword a folder cannot take (the 26
-- keywords of a Dovecot maildir folder, or a STORE rejected in that folder
-- only) is local in that folder: its assignments there live only in
-- message_labels, while the other folders keep the keyword (API keywordState
-- "partial" with localOnlyFolders). A label of an account whose server keeps
-- no keywords at all stays labels.keyword_state = 'local_only'.
CREATE TABLE label_folder_state (
  label_id   bigint      NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
  folder_id  bigint      NOT NULL REFERENCES mailbox_folders(id) ON DELETE CASCADE,
  user_id    bigint      NOT NULL,
  state      text        NOT NULL DEFAULT 'local_only' CHECK (state = 'local_only'),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (label_id, folder_id)
);
CREATE INDEX label_folder_state_folder ON label_folder_state (folder_id);

-- account_rebuilds: POST /accounts/{id}/resync {mode: rebuild} and
-- silo-server rebuild-cache. requested_at: the account's worker wipes the
-- cache on its next pass (started_at NULL); started_at: the cache was wiped
-- and the initial sync runs from scratch. restore holds what the server does
-- not keep, keyed by messages.dedupe_key (snoozes, assignments of labels that
-- are local only); it is re-attached while the messages come back and the row
-- is deleted once every folder finished its header backfill.
CREATE TABLE account_rebuilds (
  account_id   bigint      PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
  user_id      bigint      NOT NULL,
  requested_at timestamptz NOT NULL DEFAULT now(),
  started_at   timestamptz NULL,
  restore      jsonb       NOT NULL DEFAULT '{}'
);
