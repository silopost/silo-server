-- 000001_init: complete SiloPost schema (architecture plan, section 4).
-- Stays rewritable until the end of phase 2; after that, add new migrations.
--
-- Conventions: bigint identity PKs, timestamptz, user_id on every table of
-- user data (tenancy), FKs ON DELETE CASCADE from users/accounts downwards.
-- Tables from section 4.3 (threads, messages, mailbox_folders, message_locations,
-- message_labels, thread_labels) follow the plan DDL; the others are built from
-- the column lists in 4.2 in the same style.

CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS unaccent;

-- ---------------------------------------------------------------------------
-- Identity and accounts
-- ---------------------------------------------------------------------------

CREATE TABLE users (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email         citext NOT NULL UNIQUE,              -- login
  display_name  text   NOT NULL DEFAULT '',
  password_hash text   NULL,                         -- argon2id: Silo password (local) or offline verifier (imap)
  is_admin      bool   NOT NULL DEFAULT false,
  settings      jsonb  NOT NULL DEFAULT '{}',        -- density, undo_send_seconds, timezone, signature, ..., active_account_id
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE accounts (
  id                   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id              bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  email                citext NOT NULL,              -- sender address
  display_name         text   NOT NULL DEFAULT '',
  provider             text   NOT NULL DEFAULT 'generic'
                       CHECK (provider IN ('generic','gmail','microsoft')),
  imap_host            text   NOT NULL,
  imap_port            int    NOT NULL,
  imap_tls             text   NOT NULL CHECK (imap_tls IN ('implicit','starttls','none')),
  smtp_host            text   NOT NULL,
  smtp_port            int    NOT NULL,
  smtp_tls             text   NOT NULL CHECK (smtp_tls IN ('implicit','starttls','none')),
  username             text   NOT NULL,
  auth_type            text   NOT NULL DEFAULT 'password'
                       CHECK (auth_type IN ('password','xoauth2')),
  secret_enc           bytea  NOT NULL,              -- password or refresh token, AES-256-GCM
  folder_mapping       jsonb  NOT NULL DEFAULT '{}', -- manual roles: {"sent":"Sent","archive":"Archive"}
  capabilities         text[] NOT NULL DEFAULT '{}',
  keywords_supported   bool   NOT NULL DEFAULT false,
  keyword_prefix       text   NOT NULL DEFAULT '',
  sent_append          bool   NOT NULL DEFAULT false,
  sync_state           text   NOT NULL DEFAULT 'connecting'
                       CHECK (sync_state IN ('connecting','discovering','initial_sync','live','backoff','auth_failed','disabled')),
  sync_error           text   NULL,
  last_sync_at         timestamptz NULL,
  initial_sync_done_at timestamptz NULL,
  filters_since        timestamptz NULL,
  position             int    NOT NULL DEFAULT 0,
  created_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, imap_host, username)
);

CREATE TABLE sessions (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash   bytea  NOT NULL UNIQUE,
  csrf_token   text   NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  user_agent   text   NOT NULL DEFAULT '',
  ip           inet   NULL
);
CREATE INDEX sessions_user    ON sessions (user_id);
CREATE INDEX sessions_expires ON sessions (expires_at);

-- ---------------------------------------------------------------------------
-- Blobs (content-addressed; mirror = cache, local = metadata worth backing up)
-- ---------------------------------------------------------------------------

CREATE TABLE blobs (
  sha256     bytea  PRIMARY KEY CHECK (length(sha256) = 32),
  size_bytes bigint NOT NULL,
  ref_count  int    NOT NULL DEFAULT 0,
  kind       text   NOT NULL CHECK (kind IN ('mirror','local')),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blobs_unreferenced ON blobs (created_at) WHERE ref_count = 0;  -- gc

-- ---------------------------------------------------------------------------
-- Mail cache (plan DDL, 4.3)
-- ---------------------------------------------------------------------------

CREATE TABLE threads (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id          bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id       bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  subject          text   NOT NULL DEFAULT '',
  subject_norm     text   NOT NULL DEFAULT '',     -- no Re:/Fwd:/Odp:/PD:/AW:/WG:, lower, trim
  -- denormalized (maintained by mail.RecomputeThread):
  last_message_at  timestamptz NOT NULL,
  sort_at          timestamptz NOT NULL,            -- = last_message_at unless a snooze wake bumped it
  message_count    int    NOT NULL DEFAULT 0,
  unread_count     int    NOT NULL DEFAULT 0,
  has_attachments  bool   NOT NULL DEFAULT false,
  snippet          text   NOT NULL DEFAULT '',
  participants     jsonb  NOT NULL DEFAULT '[]',
  snoozed_until    timestamptz NULL,
  pending_ops      int    NOT NULL DEFAULT 0,       -- sum of messages.pending_ops
  created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX threads_account_sort ON threads (account_id, sort_at DESC, id DESC);
CREATE INDEX threads_subject_norm ON threads (account_id, subject_norm, last_message_at DESC);

CREATE TABLE mailbox_folders (
  id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  account_id         bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  user_id            bigint NOT NULL,
  name               text   NOT NULL,               -- decoded UTF-8
  name_raw           text   NOT NULL,               -- as on the server (modified UTF-7)
  delimiter          text   NULL,
  special_use        text   NULL,
  role               text   NOT NULL DEFAULT 'user',
  label_id           bigint NULL,                   -- FK to labels added below (circular)
  uidvalidity        bigint NOT NULL,
  uidnext            bigint NOT NULL DEFAULT 1,
  highestmodseq      bigint NULL,
  messages_count     int    NOT NULL DEFAULT 0,
  permanent_flags    text[] NOT NULL DEFAULT '{}',
  selectable         bool   NOT NULL DEFAULT true,
  sync_mode          text   NOT NULL DEFAULT 'sync', -- sync | ignore
  last_sync_at       timestamptz NULL,
  last_full_flags_at timestamptz NULL,
  last_error         text   NULL,
  error_count        int    NOT NULL DEFAULT 0,
  UNIQUE (account_id, name_raw)
);

CREATE TABLE sync_cursors (
  folder_id   bigint PRIMARY KEY REFERENCES mailbox_folders(id) ON DELETE CASCADE,
  phase       text   NOT NULL DEFAULT 'headers_recent'
              CHECK (phase IN ('headers_recent','headers','bodies','done')),
  low_uid     bigint NULL,                          -- backfill proceeds downwards
  done_count  int    NOT NULL DEFAULT 0,
  total_count int    NOT NULL DEFAULT 0,
  bytes_done  bigint NOT NULL DEFAULT 0,
  updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE messages (
  id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id          bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id       bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_id        bigint NOT NULL REFERENCES threads(id),
  message_id_hdr   text   NOT NULL,                 -- without <>; synthetic when absent
  dedupe_key       text   NOT NULL,                 -- message_id_hdr || ':' || size_bytes
  email_id         text   NULL,                     -- OBJECTID EMAILID / X-GM-MSGID
  in_reply_to_hdr  text   NULL,
  raw_blob_sha256  bytea  NULL REFERENCES blobs(sha256),  -- NULL until body_state <> 'full'
  body_state       text   NOT NULL DEFAULT 'headers'
                   CHECK (body_state IN ('headers','full','deferred','local')),
  size_bytes       bigint NOT NULL,                 -- RFC822.SIZE
  internal_date    timestamptz NOT NULL,            -- IMAP INTERNALDATE
  header_date      timestamptz NULL,
  from_addr        citext NOT NULL DEFAULT '',
  from_name        text   NOT NULL DEFAULT '',
  from_text        text   NOT NULL DEFAULT '',
  to_addrs         jsonb  NOT NULL DEFAULT '[]',
  cc_addrs         jsonb  NOT NULL DEFAULT '[]',
  bcc_addrs        jsonb  NOT NULL DEFAULT '[]',
  recipients_text  text   NOT NULL DEFAULT '',
  reply_to         jsonb  NULL,
  subject          text   NOT NULL DEFAULT '',
  subject_norm     text   NOT NULL DEFAULT '',     -- threading key (mail.NormalizeSubject)
  reply_prefix     bool   NOT NULL DEFAULT false,   -- subject had a Re:/Fwd: style prefix
  snippet          text   NOT NULL DEFAULT '',
  body_text        text   NOT NULL DEFAULT '',
  has_attachments  bool   NOT NULL DEFAULT false,
  is_unread        bool   NOT NULL DEFAULT true,    -- derived: no \Seen, plus pending operations
  list_id          text   NULL,
  list_unsubscribe text   NULL,
  parse_error      text   NULL,
  pending_ops      smallint NOT NULL DEFAULT 0,     -- unexecuted imap_op for this message
  restore_folder_id bigint NULL REFERENCES mailbox_folders(id) ON DELETE SET NULL,  -- where it came from before Trash/Junk
  orphaned_at      timestamptz NULL,                -- no location on the server (move in flight or deleted)
  search_tsv       tsvector NOT NULL DEFAULT ''::tsvector,
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, dedupe_key)
);
CREATE INDEX messages_thread       ON messages (thread_id, internal_date);
CREATE INDEX messages_acct_msgid   ON messages (account_id, message_id_hdr);
CREATE INDEX messages_acct_emailid ON messages (account_id, email_id) WHERE email_id IS NOT NULL;
CREATE INDEX messages_acct_date    ON messages (account_id, internal_date DESC);
CREATE INDEX messages_unread       ON messages (thread_id) WHERE is_unread;
CREATE INDEX messages_body_todo    ON messages (account_id, internal_date DESC) WHERE body_state = 'headers';
CREATE INDEX messages_orphaned     ON messages (orphaned_at) WHERE orphaned_at IS NOT NULL;
CREATE INDEX messages_tsv          ON messages USING gin (search_tsv);
CREATE INDEX messages_from_trgm    ON messages USING gin (from_text gin_trgm_ops);
CREATE INDEX messages_rcpt_trgm    ON messages USING gin (recipients_text gin_trgm_ops);

CREATE TABLE message_locations (
  folder_id  bigint NOT NULL REFERENCES mailbox_folders(id) ON DELETE CASCADE,
  uid        bigint NOT NULL,
  message_id bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  account_id bigint NOT NULL,
  user_id    bigint NOT NULL,
  flags      text[] NOT NULL DEFAULT '{}',          -- \Seen, \Flagged, ..., keywords
  modseq     bigint NULL,
  email_id   text   NULL,
  stale      bool   NOT NULL DEFAULT false,         -- after UIDVALIDITY change, until matched
  PRIMARY KEY (folder_id, uid)
);
CREATE INDEX message_locations_msg ON message_locations (message_id);

CREATE TABLE message_references (
  message_id         bigint   NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  user_id            bigint   NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id         bigint   NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  ref_message_id_hdr text     NOT NULL,
  position           smallint NOT NULL,
  PRIMARY KEY (message_id, position)
);
CREATE INDEX message_references_ref ON message_references (account_id, ref_message_id_hdr);

CREATE TABLE attachments (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  message_id   bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  blob_sha256  bytea  NULL REFERENCES blobs(sha256), -- NULL before the content is fetched
  filename     text   NOT NULL DEFAULT '',
  content_type text   NOT NULL DEFAULT 'application/octet-stream',
  size_bytes   bigint NOT NULL DEFAULT 0,            -- from BODYSTRUCTURE (encoded size, approximate)
  content_id   text   NULL,
  is_inline    bool   NOT NULL DEFAULT false,
  part_path    text   NOT NULL                       -- e.g. 1.2, allows fetching a single part
);
CREATE INDEX attachments_message  ON attachments (message_id);
CREATE INDEX attachments_blob     ON attachments (blob_sha256) WHERE blob_sha256 IS NOT NULL;
CREATE INDEX attachments_filename_trgm ON attachments USING gin (filename gin_trgm_ops);

-- ---------------------------------------------------------------------------
-- Labels (per account: system, folder, user)
-- ---------------------------------------------------------------------------

CREATE TABLE labels (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id       bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id    bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  name          text   NOT NULL,
  type          text   NOT NULL CHECK (type IN ('system','folder','user')),
  system_key    text   NULL,                        -- INBOX, SENT, ... (type = 'system' only)
  folder_id     bigint NULL REFERENCES mailbox_folders(id) ON DELETE CASCADE,
  imap_keyword  text   NULL,
  keyword_state text   NOT NULL DEFAULT 'local_only'
                CHECK (keyword_state IN ('mirrored','local_only')),
  color_bg      text   NULL,
  color_text    text   NULL,
  show_in_list  text   NOT NULL DEFAULT 'show',
  parent_id     bigint NULL REFERENCES labels(id) ON DELETE SET NULL,
  position      int    NOT NULL DEFAULT 0,
  CHECK ((type = 'system') = (system_key IS NOT NULL)),
  UNIQUE (account_id, system_key),
  UNIQUE (account_id, imap_keyword)
);
CREATE UNIQUE INDEX labels_account_name ON labels (account_id, lower(name));

ALTER TABLE mailbox_folders
  ADD CONSTRAINT mailbox_folders_label_fk
  FOREIGN KEY (label_id) REFERENCES labels(id) ON DELETE SET NULL;

CREATE TABLE message_labels (
  message_id bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  label_id   bigint NOT NULL REFERENCES labels(id)   ON DELETE CASCADE,
  user_id    bigint NOT NULL,
  PRIMARY KEY (message_id, label_id)
);
CREATE INDEX message_labels_label ON message_labels (label_id, message_id);

CREATE TABLE thread_labels (
  thread_id     bigint NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  label_id      bigint NOT NULL REFERENCES labels(id)  ON DELETE CASCADE,
  user_id       bigint NOT NULL,
  message_count int    NOT NULL,
  unread_count  int    NOT NULL DEFAULT 0,
  sort_at       timestamptz NOT NULL,
  hidden        bool   NOT NULL DEFAULT false,
  PRIMARY KEY (thread_id, label_id)
);
CREATE INDEX thread_labels_list ON thread_labels (user_id, label_id, sort_at DESC, thread_id DESC)
  WHERE NOT hidden;

-- ---------------------------------------------------------------------------
-- User metadata and workflow
-- ---------------------------------------------------------------------------

CREATE TABLE contacts (
  id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id             bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  email               citext NOT NULL,
  name                text   NOT NULL DEFAULT '',
  sent_count          int    NOT NULL DEFAULT 0,
  received_count      int    NOT NULL DEFAULT 0,
  last_interaction_at timestamptz NULL,
  is_manual           bool   NOT NULL DEFAULT false,
  UNIQUE (user_id, email)
);
-- Autocomplete matches the text-cast expression (citext has no trigram operator class).
CREATE INDEX contacts_email_trgm ON contacts USING gin ((email::text) gin_trgm_ops);
CREATE INDEX contacts_name_trgm  ON contacts USING gin (name gin_trgm_ops);

CREATE TABLE drafts (
  message_id              bigint PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  user_id                 bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id              bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  to_addrs                jsonb  NOT NULL DEFAULT '[]',
  cc_addrs                jsonb  NOT NULL DEFAULT '[]',
  bcc_addrs               jsonb  NOT NULL DEFAULT '[]',
  subject                 text   NOT NULL DEFAULT '',
  body_html               text   NOT NULL DEFAULT '',
  body_json               jsonb  NULL,              -- TipTap document
  in_reply_to_message_id  bigint NULL REFERENCES messages(id) ON DELETE SET NULL,
  attachment_ids          bigint[] NOT NULL DEFAULT '{}',
  version                 int    NOT NULL DEFAULT 1,
  updated_at              timestamptz NOT NULL DEFAULT now(),
  server_synced_version   int    NOT NULL DEFAULT 0, -- last version already present in Drafts on the server
  silo_draft_id           text   NOT NULL UNIQUE     -- X-Silo-Draft-Id header value
);
CREATE INDEX drafts_account ON drafts (account_id, updated_at DESC);

CREATE TABLE filters (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  criteria   text   NOT NULL,
  actions    jsonb  NOT NULL DEFAULT '{}',
  position   int    NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX filters_account ON filters (account_id, position);

CREATE TABLE snoozes (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_id  bigint NOT NULL UNIQUE REFERENCES threads(id) ON DELETE CASCADE,
  wake_at    timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX snoozes_wake ON snoozes (wake_at);

CREATE TABLE snooze_items (
  snooze_id        bigint NOT NULL REFERENCES snoozes(id)  ON DELETE CASCADE,
  message_id       bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  source_folder_id bigint NULL REFERENCES mailbox_folders(id) ON DELETE SET NULL,
  PRIMARY KEY (snooze_id, message_id)
);

CREATE TABLE outbound (
  id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id            bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id         bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  message_id         bigint NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
  status             text   NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued','sending','sent','failed','cancelled')),
  send_at            timestamptz NOT NULL,
  attempts           int    NOT NULL DEFAULT 0,
  last_error         text   NULL,
  smtp_response      text   NULL,
  failed_recipients  text[] NOT NULL DEFAULT '{}',
  bounced_recipients text[] NOT NULL DEFAULT '{}',
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbound_queued ON outbound (send_at) WHERE status = 'queued';

CREATE TABLE jobs (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind         text   NOT NULL,
  account_id   bigint NULL REFERENCES accounts(id) ON DELETE CASCADE,  -- NULL: send, gc, ...
  group_id     uuid   NULL,                         -- one user operation (undo)
  payload      jsonb  NOT NULL DEFAULT '{}',
  run_at       timestamptz NOT NULL DEFAULT now(),
  locked_until timestamptz NULL,
  attempts     int    NOT NULL DEFAULT 0,
  max_attempts int    NOT NULL DEFAULT 5,
  status       text   NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending','running','done','dead')),
  last_error   text   NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_general ON jobs (run_at) WHERE status = 'pending' AND account_id IS NULL;
CREATE INDEX jobs_account ON jobs (account_id, run_at, id) WHERE status = 'pending';
CREATE INDEX jobs_group   ON jobs (group_id);

-- Messages of a pending imap_op job (plan 3.6). The job payload carries only the
-- operation (op, target, flags); the messages live here so that coalescing is
-- a row insert/delete and mail.DeriveSystemLabels can overlay the pending
-- operations of a message with an index lookup instead of scanning job JSON.
-- expect_folder_id is the folder the message was in when the operation was
-- made (expectFrom). Rows go with their job (undo = DELETE jobs WHERE group_id).
CREATE TABLE op_messages (
  job_id           bigint NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  message_id       bigint NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  account_id       bigint NOT NULL,
  user_id          bigint NOT NULL,
  expect_folder_id bigint NULL,
  PRIMARY KEY (job_id, message_id)
);
CREATE INDEX op_messages_message ON op_messages (message_id);

-- Wakes idle workers (LISTEN silo_jobs) as soon as a job is inserted; the
-- payload is the account id or empty for general jobs.
CREATE FUNCTION jobs_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_notify('silo_jobs', COALESCE(NEW.account_id::text, ''));
  RETURN NULL;
END
$$;
CREATE TRIGGER jobs_notify AFTER INSERT ON jobs
  FOR EACH ROW EXECUTE FUNCTION jobs_notify();

CREATE TABLE search_history (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  query      text   NOT NULL,
  used_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (account_id, query)
);
CREATE INDEX search_history_recent ON search_history (user_id, account_id, used_at DESC);

CREATE TABLE events (
  seq        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  type       text   NOT NULL,
  payload    jsonb  NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_user_seq ON events (user_id, seq);
CREATE INDEX events_created  ON events (created_at);  -- pruned after 24 h
