# silo-server

Backend of SiloPost, a self-hosted web mail client (IMAP/SMTP). IMAP stays the
source of truth; Silo is a client. License: AGPL-3.0.

Architecture and roadmap: see `ARCHITECTURE_AND_PLAN.md` in the
[`silo`](https://github.com/silopost/silo) repository.

## Requirements

- Go 1.26+
- Postgres 17 (for `serve` and `migrate`)
- Docker (integration tests via testcontainers, image builds)
- golangci-lint v2 (optional, for `make lint`)

## Commands

```sh
silo-server serve [--migrate=false]   # HTTP server; applies migrations first by default
silo-server migrate up                # apply pending migrations
silo-server migrate down --yes        # revert everything (drops all data)
silo-server migrate version           # current schema version
silo-server rotate-secrets           # re-encrypt account secrets after a key change
silo-server reconcile [--account ID] # report drift of derived mail data (threads, labels)
silo-server rebuild-cache --account ID|--all [--yes] # drop the cached mail, sync it again
silo-server user add|passwd|admin|list   # manage Silo users (see below)
silo-server export-metadata [--out FILE|-] [--user EMAIL] [--no-local-blobs]   # metadata backup (JSON)
silo-server import-metadata --in FILE|- [--merge]   # restore a metadata backup
silo-server version
```

`migrate` needs only `SILO_DATABASE_URL`. Migrations run under a Postgres
advisory lock, so several instances can start at once.

`rotate-secrets` rotates the key protecting stored mailbox passwords and TOTP secrets: set
`SILO_SECRET_KEY` to the new key and `SILO_SECRET_KEY_PREVIOUS` to the old one,
run the command (it needs both plus `SILO_DATABASE_URL`, is safe to repeat and
reports how many accounts and TOTP secrets were re-encrypted), then drop the previous key.

## Run

```sh
export SILO_DATABASE_URL=postgres://silo:silo@localhost:5432/silo?sslmode=disable
export SILO_PUBLIC_URL=http://localhost:8080
export SILO_SECRET_KEY=$(head -c 32 /dev/urandom | base64)
export SILO_DEFAULT_IMAP_HOST=imap.example.com
make run
curl localhost:8080/healthz        # {"status":"ok"}
curl localhost:8080/api/v1/meta
```

Other targets: `make build`, `make test`, `make lint`, `make sqlc`, `make openapi`.

Version: `GET /meta` reports `internal/version.Version`, set at link time. `make build` takes it from `git describe` (tag `v0.2.0` gives `v0.2.0`); a release image needs `--build-arg VERSION=v0.2.0` (CI builds `edge-<sha>`).

Docker: `docker build --target prod -t silo-server .` (or `--target dev` for
hot reload with air; mount the repo at `/src`).

## Features (Faza 4)

What phase 4 of the plan added; the details are in the sections linked.

- **Search** (`GET /threads?q=`, Gmail syntax parsed on the server, suggestions and
  history, `indexing`/`approximate`): bullet "Search" in [Configuration](#configuration).
- **Snooze** (`POST/DELETE /threads/{id}/snooze`, `GET /snooze/presets`, folder of role
  `snoozed`, job `snooze_wake`): bullet "Snooze" in [Configuration](#configuration).
- **Schedule send** (`sendAt`, list `SCHEDULED`, `scheduledAt`/`outboundId` on threads):
  bullet "Schedule send" in [Configuration](#configuration).
- **Filters** (`/filters` CRUD, `filter_eval` on ingest, `POST /filters/{id}/apply`):
  bullet "Filters" in [Configuration](#configuration).
- **Settings** (`GET/PATCH /settings`, `settings.updated`): bullet "Settings" in
  [Configuration](#configuration).
- **Compose media** (inline `cid:` uploads, thumbnails, reply/forward with the quoted
  HTML and the original attachments, `Draft.warnings`): bullet "Compose media" in
  [Configuration](#configuration).
- **Labels as IMAP keywords** (`keywordState` `mirrored|local_only|partial`,
  `localOnlyFolders`, the maildir limit `SILO_SYNC_KEYWORD_FOLDER_LIMIT`, `PATCH
  /labels/{id}` with `name`/`parentId`/colours): [Labels and IMAP keywords](#labels-and-imap-keywords).
  `parentId` moves a user label under another user label of the account (`null`: top
  level; itself, a descendant or anything else is `422 invalid_parent`); the name and
  the keyword stay, since the keyword is the slug of the leaf name only (`labels.name`
  holds no path; renaming assigns a new keyword from the new leaf name).
- **Cache rebuild** (`POST /accounts/{id}/resync {mode: rebuild}`, `silo-server
  rebuild-cache`): [Cache rebuild](#cache-rebuild).
- New migrations and background jobs: [Migrations and background jobs](#migrations-and-background-jobs).

Not done in Faza 4: PDF thumbnails (no cgo-free renderer: `415 thumbnail_unsupported`),
bounce/DSN handling.

## Features (Faza 5)

What phase 5 of the plan added; the details are in the sections linked.

- **Several accounts per user, isolated users** (`SILO_MAX_ACCOUNTS_PER_USER`, default 5;
  over it `409 account_limit_reached`): [Multiple accounts](#multiple-accounts).
- **Mode `local`, TOTP, admin panel, `user` command** (`SILO_AUTH_MODE=local`, `POST
  /session/password`, `/totp/*`, `/admin/users|accounts|jobs`): [Users, mode `local` and
  the admin panel](#users-mode-local-and-the-admin-panel).
- **Sessions**: `GET /sessions`, `DELETE /sessions/{id}` and `DELETE /sessions?others=true`
  (sign out everywhere else, `204`); a stream of a session that ends gets
  `session.expired`: bullet "Events" in [Configuration](#configuration).
- **Backup and import of metadata** (`export-metadata`, `import-metadata [--merge]`,
  format `silo-metadata` v1, job `metadata_restore`): [Backup and restore](#backup-and-restore).
- **S3Store** (`SILO_BLOB_BACKEND=s3`, `SILO_BLOB_S3_*`): [S3Store](#s3store).
- **Hardening**: trusted proxies and client IP, rate limits (`429 rate_limited`,
  `Retry-After`), at most 8 event streams per user (`stream.limit`), security headers and
  CSP, the security log for fail2ban, anti-SSRF dialing (`internal/netx`): [Hardening](#hardening).
- **Account autoconfiguration** (`POST /accounts/discover`: ISPDB, autoconfig, SRV,
  guesses): [Account autoconfiguration](#account-autoconfiguration).
- **OAuth2 accounts** (Google, Microsoft 365; XOAUTH2 for IMAP and SMTP, `authType`,
  re-authorization after `auth_failed`) and **Gmail in generic mode** (`\All`, archive as
  expunge from INBOX): [OAuth2](#oauth2-gmail-microsoft-365).
- **Upgrade test** (`scripts/upgrade-test.sh`, previous release -> working tree on the
  same database): [Upgrades](#upgrades).
- **Large mailboxes**: `TestInitialSync` with `SILO_SYNC_TEST_MESSAGES=200000` reports
  the memory of the initial sync: [IMAP integration tests](#imap-integration-tests).
- New migrations (`000050`-`000052`) and the job `metadata_restore`:
  [Migrations and background jobs](#migrations-and-background-jobs).

### Deferred to 1.1

- **`provider=gmail` mode** (`X-GM-LABELS`, `X-GM-MSGID`, `X-GM-THRID`): Gmail accounts
  sync in generic mode (folders, `\All` with dedup); Gmail labels are not mapped to Silo
  labels and threads come from Silo's own threading. No test account and no X-GM
  simulator (Dovecot does not offer `X-GM-EXT-1`).
- **ManageSieve**: filters run in Silo only (on new INBOX mail while Silo runs); they are
  not exported to the mail server as Sieve scripts.
- **OAuth2 for native clients**: the sign-in is bound to the browser that started it by
  the `silo_oauth_state` cookie (protection against login CSRF), so a bearer client that
  opens an external browser gets `oauth_state_invalid`; it needs a variant with the
  app's redirect URI (silo-mobile, Faza 6).

## Multiple accounts

A user has up to `SILO_MAX_ACCOUNTS_PER_USER` mail accounts (default 5; `POST
/accounts` over it, or an OAuth2 sign-in adding one, is `409 account_limit_reached`).
Accounts are ordered by `position`; `settings.activeAccountId` (`PATCH /settings`) is
the account the client opens (unset: the first one). Every list, label, count and event
names its account: `label.counts`, `thread.updated` and `account.*` carry `accountId`.

- **From**: `PUT /drafts/{id}` with `accountId` sends a new message from another account
  of the user. The draft moves there as a new draft (new `id`, `messageId` and
  `threadId`, `draft.updated` with `previousDraftId`; the copy in the old account's
  Drafts is deleted); a reply or forward stays with its account (`409
  draft_account_locked`).
- **Isolation**: ids of another user's objects in a path answer `404` exactly like ids
  that do not exist; in a body (`addLabelIds`, `parentId`, attachments, folders) they
  are `400`/`422` like unknown ids; events and polls only ever carry the user's own
  data. `internal/http/api` `TestUserIsolation` checks every endpoint both ways.

## Configuration

All settings are `SILO_*` environment variables; the full table (names,
defaults, meaning) is in section 2.5 of the architecture plan. `serve` checks
the whole configuration at startup and reports every problem at once. Sizes
accept `50MB` style values, rates `5MB/s`, durations `5m`. Worth knowing:

- Required for `serve`: `SILO_DATABASE_URL`, `SILO_PUBLIC_URL`, `SILO_SECRET_KEY`
  (32 random bytes, base64) and, in `SILO_AUTH_MODE=imap` (default),
  `SILO_DEFAULT_IMAP_HOST`.
- `*_TLS=none` is only accepted with `SILO_ALLOW_INSECURE_TRANSPORT=true`.
- Blobs (raw messages, attachments, uploads): `SILO_BLOB_BACKEND=fs` (default) stores under
  `SILO_BLOB_FS_DIR`, `s3` in a bucket (see [S3Store](#s3store)).
- Background jobs: `serve` runs a Postgres-backed job queue; `SILO_WORKER_CONCURRENCY`
  (default 4) sets the size of the general worker pool. Jobs are woken by
  `LISTEN/NOTIFY` with a 5 s polling fallback.
- IMAP sync (`internal/sync`): `serve` runs one account worker per account that is
  not disabled, each with up to `SILO_SYNC_MAX_CONNECTIONS_PER_ACCOUNT` connections
  (default 3, minimum 2: `sync` + `idle`; the third is reserved for write-back).
  `SILO_SYNC_INITIAL_CONCURRENCY` (default 2) limits how many accounts connect and
  backfill at the same time, `SILO_SYNC_POLL_INTERVAL` (default `5m`) is the folder
  poll/STATUS sweep, `SILO_SYNC_BACKFILL_RATE` (default `5MB/s`) paces the body
  backfill per account. Messages above `SILO_MIRROR_MAX_MESSAGE_BYTES` (default
  `50MB`) keep headers only (`deferred`); `SILO_MIRROR_QUOTA_PER_ACCOUNT` (default
  `0` = no limit) stops the body backfill of an account at that many mirrored bytes.
  `SILO_SYNC_KEYWORD_FOLDER_LIMIT` (default `26`, `0` = no limit) is how many distinct
  keywords one folder keeps (see "Labels and IMAP keywords").
- Write-back (`internal/ops`): label changes (`POST /threads/modify`, `/messages/modify`,
  `/threads/trash`, `/untrash`, `/delete`) are applied locally at once and queued as
  `imap_op` jobs run by the account's worker. Folder moves wait `SILO_UNDO_WINDOW`
  (default `10s`), flags go at once; queued changes are coalesced, and jobs wait
  (up to 7 days) while the account is offline. `POST /operations/{id}/undo` cancels an
  operation while its jobs wait (the server never sees it); once a job was claimed or
  ran, the server queues the inverse itself (messages back to the folders they came
  from, flags and user labels changed back) as a new operation with its own
  `operationId`/`undoUntil`. Inverses live in the `operations` table for 24 h (purged by
  the hourly `blob_gc` job); older operations answer `409 undo_expired`, a permanent
  delete outside its window `409 operation_not_undoable`.
- Drafts and uploads (`internal/drafts`): `POST /uploads` streams one multipart file
  (up to `SILO_MAX_ATTACHMENT_BYTES`, default `25MB`, else `413 upload_too_large`) to
  the blob store as `u_…`; uploads not attached to a draft are removed after 24 h by the
  hourly `blob_gc` job, which also deletes unreferenced blobs. `POST /drafts` (new,
  `reply`, `replyAll`, `forward` with prefilled recipients and subject), `GET`/`PUT`/
  `DELETE /drafts/{id}`: a draft is a local message with the `DRAFT` label (`d_` id =
  its `m_` id); `PUT` takes the edited `version` (`409 draft_version_conflict`) and
  pins `attachmentIds`. At least 30 s after the last save an `imap_op` `append` job
  writes exactly one copy to the Drafts folder (`X-Silo-Draft-Id`, `\Draft \Seen`,
  older copies expunged; the folder is created if missing); sync recognises the copy by
  that header. Drafts written by other programs open via `GET /drafts/{id}`.
  `PUT` with `"syncNow": true` (the save when the compose window closes) makes that job
  due at once. A `409 draft_version_conflict` carries `currentVersion` and `current`
  (the draft as saved meanwhile); an invalid address is `400 validation_failed` with
  `address` and `field` (`to`/`cc`/`bcc`). Messages and threads carry `draftId`
  (`d_…`, the newest draft of a thread) for opening them in compose. Drafts never
  saved and still empty (version 1, no recipients, subject, body or attachments, no
  server copy) are deleted after 24 h by `blob_gc`. The limit is `maxAttachmentBytes`
  in `GET /meta`; `413 upload_too_large` carries `limit` and `size`.
- Compose media (plan 5.1/5.6, Faza 4):
  - Inline pictures: `POST /uploads` with `inline=true` (a form field before the file,
    or `?inline=true`) stores an `image/*` upload (`422 inline_not_image` otherwise)
    with a server-generated Content-ID; the answer is
    `{id, filename, size, contentType, inline: true, cid: "<hex>@silo", thumbnail}`
    (`inline: false, cid: null` for plain uploads). The client puts
    `<img src="cid:<cid>">` in `bodyHtml` and the id in `attachmentIds` like any upload;
    drafts list `attachments[].inline`/`cid`. Inline pictures do not set
    `has_attachments`. The sent message is
    `mixed( related( alternative(text, html), inline… ), attachments… )` (levels with one
    part left out); an inline upload the HTML no longer refers to is sent as a plain
    attachment, as Gmail does. `GET /attachments/{id}` also serves uploads (`u_…`), and
    `GET /messages/{id}/html` of a draft resolves its `cid:` pictures.
  - Received mail: parts with a Content-ID that are not `attachment` are `inline`
    (stored at ingest from BODYSTRUCTURE and when the body is downloaded), do not count
    for `has:attachment`, and `cid:` in the HTML points to
    `/api/v1/attachments/a_…?inline=1` (unknown cids are dropped).
  - Replies and forwards: a body not downloaded yet is fetched first (up to 10 s); the
    quote is the sanitised HTML of the original (its text without HTML), with the inline
    pictures it shows copied to the draft under the same cid. A forward also carries the
    original's attachments (new `attachments` rows sharing the blob, `ref_count` + 1;
    their ids belong in `attachmentIds`), also for the filter action `forwardTo`.
    `POST /drafts` returns `warnings`: `quote_unavailable` (no body: the quote is the
    snippet), `attachments_unavailable` (parts that could not be downloaded were left
    out), `attachments_too_large` (parts over `SILO_MAX_ATTACHMENT_BYTES` together).
  - Thumbnails: `GET /attachments/{id}/thumbnail?size=s|m` (`a_…` or `u_…`; s = 64 px,
    m = 256 px on the longer side, never enlarged) for png, jpeg, gif (first frame) and
    webp up to 20 megapixels; JPEG, or PNG when the picture has transparency;
    `Cache-Control: private, immutable`. Made on the first request (pure Go:
    `image/*`, `golang.org/x/image`) and cached in the blob store (table `thumbnails`,
    one per source blob and size; `blob_gc` drops them with their source). Other types
    are `415 thumbnail_unsupported` — PDF included, since rendering PDF needs cgo; an
    attachment not downloaded yet is fetched first (`503 content_not_available` as for
    the bytes). Attachments of messages and uploads carry `thumbnail: bool`.
- Sending (`internal/drafts`, plan 3.9): `POST /drafts/{id}/send` (`{sendAt?}`) builds
  the message at once (Bcc only in the envelope), stores it and queues it: the message
  is `SCHEDULED`, an `outbound` row (`ob_…`) is `queued` and a `send` job runs at
  `undoUntil` = now + `undo_send_seconds` (5/10/20/30, default 5) or `sendAt`. Errors:
  `422 no_recipients` / `invalid_address` (with `address` and `field`),
  `413 message_too_large` (over `SILO_MAX_ATTACHMENT_BYTES`; with `limit` and `size`),
  `409 send_in_progress`. `POST /outbound/{id}/cancel`
  (undo send) deletes the unclaimed job and returns the draft unchanged (same
  `version`); once the job is claimed it is `409 send_in_progress`. The job submits
  over SMTP (`go-smtp`; 465 implicit TLS or STARTTLS, required unless
  `SILO_ALLOW_INSECURE_TRANSPORT`; AUTH PLAIN, else LOGIN; the server's `SIZE` is
  checked): 4xx/network errors retry after 30 s, 2 min, 10 min, 30 min, 5xx fails at
  once, recipients refused at `RCPT` go to `failed_recipients`. A failed send is a draft
  again (`outbound.status` event with `error` and `draftId`; the draft's `lastSend` keeps
  `{outboundId, status, error, failedRecipients, updatedAt}` until the next send). A
  send left in `sending` by a crash (no update for 2 × the SMTP timeout, its job not
  running) is failed by `blob_gc` with `error: "interrupted"` and is **not** retried
  automatically (it may have left; the user decides). After sending the message is `SENT`,
  its Drafts copy is removed, the original gets `\Answered` (reply) or `$Forwarded`
  (forward), `contacts.sent_count` grows and, when the account's `sentAppend` is on
  (default for generic servers), an `imap_op` `append` saves it in Sent with `\Seen`;
  otherwise the server's own copy in Sent is attached by `Message-ID`.
  `GET /contacts/autocomplete?q=` suggests up to 8 recipients.
- Schedule send (plan 3.9 step 3): `sendAt` beyond the undo-send window schedules the
  message (at most a year ahead). It stays local until it leaves: its copy in the Drafts
  folder is removed when it is scheduled (no other client can send it from there) and
  saved again if the schedule is cancelled or the send fails. The "Scheduled" list is
  `GET /threads?labelId=SCHEDULED`; threads (list and `GET /threads/{id}`) carry
  `scheduledAt` (the `outbound.send_at` of a queued/sending send still ahead) and
  `outboundId` for `POST /outbound/{id}/cancel`. Precision: the job poll (±30 s DoD).
- Snooze (`internal/ops`, plan 3.10): `POST /threads/{id}/snooze` `{until}` (≥ 30 s,
  ≤ 1 year ahead, else `422 invalid_snooze`; a thread without mail in INBOX is
  `409 not_in_inbox`) answers `{thread, operationId, undoUntil}`: INBOX turns `SNOOZED`
  at once and after `SILO_UNDO_WINDOW` an `imap_op` moves the messages to the folder of
  role `snoozed` (`Snoozed`, created by Silo on the first snooze like Archive; another
  name can be mapped with `PUT /accounts/{id}/folders`, which also detects `Snoozed` by
  name) with the keyword `$SiloSnoozed` when the server keeps keywords, so other clients
  do not show them in their inbox. Snoozing again changes the time; the operation is
  undone by `/operations/{id}/undo`. `snooze_wake` (queued every 30 s and by ingest when a
  new message arrives in a snoozed thread) moves due threads back to INBOX at once
  without `$SiloSnoozed`, the newest message unread, the thread on top
  (`threads.sort_at = now()`), and drops snoozes whose messages another client took out of
  Snoozed; `DELETE /threads/{id}/snooze` wakes now (not undoable; `409 not_snoozed`).
  `GET /snooze/presets[?now=]` lists `later_today`/`tomorrow`/`this_weekend`/`next_week`
  computed in `users.settings.timezone`. `SNOOZED` cannot be set by `modify`; threads and
  `thread.updated` carry `snoozedUntil`. Waking needs a running Silo (mail waits safely in
  Snoozed); `snooze_items` point at messages, not UIDs, so a snooze survives a
  UIDVALIDITY change or a re-created Snoozed folder.
- Filters (`internal/filters`, plan 3.11): `GET /filters?accountId=` (in `position`
  order), `GET/PUT/DELETE /filters/{id}`, `POST /filters` → `Filter`
  `{id: f_…, accountId, name, criteria, criteriaNormalized, actions, enabled, position,
  createdAt, updatedAt}`. `criteria` is a query of the search syntax (the same parser,
  AST and semantics as `GET /threads?q=`; `422 invalid_query` likewise; empty or only
  `in:anywhere` is `422 invalid_filter`); `actions`: `archive` (skip the inbox),
  `markRead`, `star`, `important`, `addLabelIds` (user labels of the filter's account,
  else `422 invalid_label`), `trash` (not with `archive`), `forwardTo`; at least one.
  `PUT` replaces the whole filter; `position` (0-based, in `POST`/`PUT`) moves it and the
  others shift — that is how filters are reordered. Matching runs on the server only:
  ingest queues `filter_eval` for messages that appear in INBOX for the first time with
  `internal_date >= accounts.filters_since` (set when the account first goes live; older
  mail is only touched by apply); the enabled filters run in memory (`search.Matcher`)
  in position order and the actions of every matching filter are applied together
  (trash wins over archive) as `imap_op` operations **without the undo window** (no
  `operationId`; `operation.failed` still reports a job given up). A filter with a word
  of the body (a term without a key) or `forwardTo` waits up to 10 s from ingest for the
  body (the sync downloads new INBOX bodies at once, else a priority fetch), then runs
  on the headers. A new message archived or trashed by a filter does not wake its
  snoozed thread; one left in INBOX does. `forwardTo` sends a copy at once through the
  compose path (a forward draft, sent without the undo-send window, saved in Sent),
  never of the account's own mail and never by apply. `POST /filters/{id}/apply` →
  `202 {jobId, matchedEstimate}` queues `filter_apply`: the same AST as SQL over the
  account's messages (drafts and, unless the criteria name them, Spam/Trash left out),
  in batches of 500 operations without undo, then the event `filter.applied
  {filterId, accountId, messages, threads}`. Migration `000007` adds `filters.name`,
  `enabled`, `updated_at`.
- Settings: `GET /settings` and `PATCH /settings` (JSON merge: only the fields sent
  change; `activeAccountId: null` clears it) over `users.settings`; the same object is
  `settings` of `GET /session`. Fields: `density` (`default|comfortable|compact`),
  `undoSendSeconds` (5/10/20/30: the undo-send window), `signature` (HTML ≤ 32 KiB,
  stored sanitized like message HTML), `timezone` (IANA name: `before:`/`after:` of
  search and filters, snooze presets; else `422 invalid_timezone`),
  `keyboardShortcutsEnabled`, `conversationView`, `activeAccountId` (an account of the
  user), `desktopNotifications` (the preference is shared; each client asks the system
  for its own permission); other bad values are `422 invalid_setting`. The answer carries
  `updatedAt` (`users.settings.updated_at`); other sessions get `settings.updated
  {updatedAt}` and reload.
- Sessions and accounts (`internal/auth`, `internal/accounts`): `POST/GET/DELETE /session`, `GET /sessions`, `DELETE /sessions/{id}`, `DELETE /sessions?others=true` (every session but the current one), `GET/POST /accounts`, `POST /accounts/test`, `GET/PATCH/DELETE /accounts/{id}`, `GET /accounts/{id}/folders`, `POST /accounts/{id}/resync`, `GET /accounts/{id}/sync-status`, `GET /meta`. Accounts carry `position` and are listed in that order (`GET /session`, `GET /accounts`). Migration `000005` turns `sent_append` on for generic accounts created before sending existed. The full list is `api/openapi.yaml`.
- Reading the mailbox (`internal/mailview`): `GET /labels` (counters, pseudo label
  `UNREAD`), `POST /labels` (user label with an IMAP keyword, or a folder: `CREATE`
  queued as an `imap_op` job), `PATCH`/`DELETE /labels/{id}` (a rename moves the
  keyword on the server, a folder is deleted only when empty), `GET /threads`
  (keyset pagination, `labelId`, search `q`), `GET /threads/{id}`,
  `GET /messages/{id}/html` (sanitised, immutable, CSP), `/messages/{id}/raw` and
  `GET /attachments/{id}`. A body that is still `headers`/`deferred` is downloaded
  on demand (up to 10 s, then `503 content_not_available` + `Retry-After`).
- Search (`internal/search`, plan 3.12): `GET /threads?q=` takes the Gmail syntax, parsed on
  the server: words (prefix match, case and accents ignored: `lodz` finds `Łódź`),
  `"phrases"`, `OR`/`|`, `AND` or juxtaposition, `-`, `( )`, `{a b}` (= `a OR b`), and
  `from: to: cc: bcc: subject: label: in: is: has: filename: list: deliveredto:
  rfc822msgid: before: after: older: newer: older_than: newer_than: larger: smaller:`
  (values may be `"phrases"` or groups: `subject:(a b)`). An unknown key or a bad value
  (`before:2024/13/40`) is a plain word; dates are `YYYY/MM/DD` or `YYYY-MM-DD` at
  midnight in `users.settings.timezone` (UTC without it), ages `Nd`/`Nm`/`Ny`, sizes bytes
  or `100K`/`5M`/`1G`. Spam and Trash are left out unless the query names `in:spam`,
  `in:trash` or `in:anywhere`; `labelId` narrows the search like `in:`. Results are
  threads newest first, at most 2000 (`resultSizeEstimate`); a search over 3 s answers an
  empty page with `approximate: true`; `indexing.done` (with `q` only) is the share of
  messages whose body is downloaded (`has:attachment` and headers work before that).
  `422 invalid_query`: a closing bracket without its opening one, or more than 64 terms /
  16 nested brackets (unclosed brackets and quotes are accepted, as in Gmail). The first
  page of a search saves the query (as typed) in the account's history (50 kept);
  `GET /search/suggest?q=` returns `{kind, text, label}` suggestions (history, contacts,
  labels, operators; `text` is the whole query to put in the box) and
  `DELETE /search/history[?query=]` forgets one or all. `search_tsv` is built by the
  application tokenizer (letters/digits runs, folded; e-mail addresses split into words);
  migration `000006` marks rows for a background re-index at start-up. The in-memory
  `search.Matcher` (filters) evaluates the same AST; a property test checks it against SQL.
- Events: `GET /events` is a Server-Sent Events stream (24 h replay with
  `Last-Event-ID`, `: ping` every 25 s, `resync` when the id is too old), and
  `GET /events/poll?after=` the polling fallback. Rows of the `events` table are
  announced by `NOTIFY silo_events` at commit; `X-Silo-Client` is echoed as `origin`.
  `operation.failed` carries `kind` (`move`, `flags`, `labels`, `expunge`, `append`,
  `delete_draft`, `create_folder`, `delete_folder`) so the client can name the action.
  `filter.applied` ends `POST /filters/{id}/apply`; `settings.updated` follows `PATCH /settings`.
  The session is checked with every `: ping`: once it is signed out (`DELETE /session`),
  revoked (`DELETE /sessions/{id}`), expired or its user disabled, the stream sends
  `event: session.expired` (`data: {}`) and closes; sign in again. At most 8 streams
  per user are open at once; a stream over the limit answers `200 text/event-stream`
  with one `event: stream.limit` (`data: {"retryAfter":30}`, plus `retry: 30000`) and
  closes (an `EventSource` cannot see a `429`): close the `EventSource` and poll
  `/events/poll` instead. `label.counts` with every label of an account follows
  `account.status` `live` and the end of its initial sync.
- Benchmark (nightly workflow): `SILO_BENCH_INBOX=true go test ./internal/mailview
  -run TestInboxListBenchmark -v` loads 100k threads / 300k messages and checks
  p95 < 50 ms and the `thread_labels_list` plan of the inbox query.
  `SILO_BENCH_SEARCH=true go test ./internal/mailview -run TestSearchBenchmark -v` loads
  1M messages (`SILO_BENCH_MESSAGES`) and checks search p95 < 500 ms for one word and `from:`.
- Sessions: the `silo_session` cookie is `Secure` when `SILO_PUBLIC_URL` is `https://`
  (plain `http://` is for local development only). Mutating requests need the
  `X-Silo-CSRF` header and an `Origin` equal to `SILO_PUBLIC_URL` or the served host.
  Sign-in is throttled per client IP (`SILO_RATE_LIMIT_AUTH`, see [Hardening](#hardening))
  and per login (burst 8, one more every 15 s), and answers after a constant minimum time.
  Browser sessions last 30 days idle / 90 days at most.
- Bearer sessions (native clients, ADR 0007 in `silo`): `POST /session` with
  `"client": {"kind": "bearer", "name": "Pixel 8"}` returns `token` in the body (shown
  once) and sets no cookie. Send it as `Authorization: Bearer <token>` on every request,
  including `GET /events` and attachment downloads; no `X-Silo-CSRF` or `Origin` check
  applies. Tokens are opaque (only the SHA-256 is stored in `sessions`), bound to their
  channel (a cookie token is not accepted as bearer and vice versa), last 90 days idle /
  365 days at most, and are listed with `kind` and `clientName` in `GET /sessions`.
- Custom mail servers (`SILO_ALLOW_CUSTOM_SERVERS=true`) must resolve to public
  addresses unless listed in `SILO_CUSTOM_SERVER_ALLOWLIST` (host names, IPs or CIDRs).
  The check is repeated on the address actually dialled (IMAP, SMTP, connection test):
  a host that resolves to an internal address later (DNS rebinding) is not contacted.
  In `SILO_AUTH_MODE=local` custom servers are always allowed (see below).
- Account autoconfiguration (`POST /accounts/discover`): `SILO_AUTOCONFIG_ISPDB_URL`
  (ISPDB base URL, `off` to disable); see [Account autoconfiguration](#account-autoconfiguration).
- OAuth2 accounts (Gmail, Microsoft 365): `SILO_OAUTH_GOOGLE_CLIENT_ID` /
  `_CLIENT_SECRET`, `SILO_OAUTH_MICROSOFT_CLIENT_ID` / `_CLIENT_SECRET` /
  `_TENANT`; see [OAuth2](#oauth2-gmail-microsoft-365).
- Logging: `SILO_LOG_FORMAT` (`json`/`text`), `SILO_LOG_LEVEL`; listen address: `SILO_HTTP_ADDR`.
- Reverse proxy, rate limits, headers: `SILO_TRUSTED_PROXIES`, `SILO_RATE_LIMIT_API`,
  `SILO_RATE_LIMIT_AUTH`, `SILO_HSTS`; see [Hardening](#hardening).

## Hardening

### Reverse proxy and client IP

The client IP feeds the sign-in throttling, the rate limits and the logs.
`X-Forwarded-For` and `X-Real-IP` are honoured only when the connection comes
from a proxy listed in `SILO_TRUSTED_PROXIES` (comma-separated IPs or CIDRs,
e.g. `172.18.0.0/16` for a Docker network, `127.0.0.1` for a proxy on the host).
The client is then the right-most `X-Forwarded-For` address that is not a
trusted proxy; an unparsable entry falls back to the proxy's address. With
the default (empty), the headers are ignored: set the variable whenever
Silo runs behind Caddy, nginx, Traefik or a load balancer, otherwise every
request counts against the proxy's own IP.

### Rate limits

Token buckets per client IP, in memory (per process):

| Variable | Default | Applies to |
|---|---|---|
| `SILO_RATE_LIMIT_API` | `600/m` | `/api/v1/*` except the endpoints below |
| `SILO_RATE_LIMIT_AUTH` | `20/m` | `POST /session`, `POST /session/password`, `/totp/*` |

The format is `N/s`, `N/m`, `N/h` or `N/<duration>` (`20/90s`): `N` requests at
once, refilled evenly over the period; `off` disables a limit. `20/m` is the
sign-in throttling of earlier releases (burst 20, one more every 3 s); the
per-login limit of sign-in stays as it was. `/healthz` is not limited and
`GET /events` (a long-lived stream) is limited to 8 open streams per user
instead (over it: `event: stream.limit`, see Events). Over the other limits the
answer is `429` `application/problem+json` with
`code: "rate_limited"` and `Retry-After` in seconds.

### Response headers

Every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options:
DENY`, `Referrer-Policy: no-referrer`, `Permissions-Policy: camera=(),
microphone=(), geolocation=()`, `Cross-Origin-Resource-Policy: same-origin`,
`Cache-Control: no-store` and `Content-Security-Policy: default-src 'none';
frame-ancestors 'none'`. Exceptions: `GET /messages/{id}/html`,
`/messages/{id}/raw` and `/attachments/{id}` stay `private, immutable`
(their content never changes); the message HTML has its own policy
`default-src 'none'; img-src data: https: http: cid:; style-src 'unsafe-inline';
form-action 'none'; frame-ancestors 'self' <SILO_PUBLIC_URL origin>` (the web
app shows it through `srcdoc` in a sandboxed iframe and blocks remote images
itself) and attachments `default-src 'none'; sandbox`.
`Strict-Transport-Security: max-age=31536000; includeSubDomains` is sent when
`SILO_PUBLIC_URL` is `https://`; set `SILO_HSTS=false` when the proxy already
sends it or other services on subdomains are not on HTTPS yet.

### Security log and fail2ban

Each failed sign-in or re-authentication, rejected TOTP code, CSRF/Origin
rejection, other `403` and every `429` writes one `WARN` line with the message
`security event` and the fields `event` (`auth_failed`, `totp_failed`,
`csrf_rejected`, `forbidden`, `rate_limited`), `ip` (the client after
`SILO_TRUSTED_PROXIES`), `path` and `login` (masked: `j***@example.com`;
passwords and tokens are never logged), in this order:

```
time=2026-11-05T10:00:00.000Z level=WARN msg="security event" event=auth_failed ip=203.0.113.7 path=/api/v1/session login=j***@example.com
{"time":"2026-11-05T10:00:00Z","level":"WARN","msg":"security event","event":"auth_failed","ip":"203.0.113.7","path":"/api/v1/session","login":"j***@example.com"}
```

A missing or expired session (`401 unauthenticated`) is not logged. A fail2ban
filter (`/etc/fail2ban/filter.d/silo.conf`) for `SILO_LOG_FORMAT=text`:

```ini
[Definition]
failregex = ^.*msg="security event" event=(?:auth_failed|totp_failed|csrf_rejected) ip=<ADDR>
```

and for `SILO_LOG_FORMAT=json` (the default):

```ini
[Definition]
failregex = ^.*"msg":"security event","event":"(?:auth_failed|totp_failed|csrf_rejected)","ip":"<ADDR>"
```

Point the jail at the container log (e.g. `logpath =
/var/lib/docker/containers/*/*-json.log` for the `json-file` driver, or
`backend = systemd` with `journalmatch = CONTAINER_NAME=silo-server` for
journald). Add `rate_limited` to the alternatives for a stricter jail; a
client behind a shared NAT may hit the API limit legitimately.

### Upgrades

`scripts/upgrade-test.sh` checks the upgrade path end to end with Docker:
the previous release (`PREVIOUS_TAG`, default the last `v*` tag on `main`) is
built from `git archive`, migrated, started against Postgres 17 and Dovecot,
seeded through the API (sign-in, four messages, a user label on a thread and,
from `v0.4.0` on, a filter and a thread snoozed for an hour);
then the image of the working tree migrates the same database (the schema
version must grow), starts, signs the same user in, must list the same INBOX
with the label, still list the snoozed thread under `SNOOZED` with the same wake
time, still have the filter, report its own version in `/meta` and pass `reconcile`.
Containers, volume, network and images are named `silo-upg-*` and removed at
the end (`KEEP=1` keeps them). It runs in CI as `.github/workflows/upgrade.yml`
(also by hand with a `previous_tag` input).

```sh
PREVIOUS_TAG=v0.4.0 scripts/upgrade-test.sh
```

## Labels and IMAP keywords

User labels live in Postgres (`message_labels`) and, when the server keeps keywords
(`\*` in the INBOX `PERMANENTFLAGS`, `keywordsSupported` in `sync-status`), are also
written on the messages as IMAP keywords, so Thunderbird shows them as tags and a
cache rebuild gets them back (plan 3.7).

- Keyword = ASCII slug of the name (`Praca/Projekty` -> `Praca_Projekty`, `Zażółć` ->
  `Zazolc`) with the account's `keyword_prefix` (default empty). A slug already used by
  another label or listed by the server in a folder's `PERMANENTFLAGS` (a tag of another
  client) gets `_2`, `_3`, ...: the mapping is explicit in `labels.imap_keyword`.
- Keywords of other clients become labels named after the keyword (prefix stripped);
  Thunderbird `$label1`-`$label5` become "Ważne", "Praca", "Osobiste", "Do zrobienia",
  "Później". `$Forwarded`, `$MDNSent`, `$Junk`, `$NotJunk`, `NonJunk`, `$Important`,
  `$Phishing` and `$SiloSnoozed` never become labels. A tag set elsewhere shows up within
  seconds in INBOX (IDLE) and within `SILO_SYNC_POLL_INTERVAL` in other folders.
- Rename and delete queue one flags operation over every message of the label in every
  folder (`+new -old`, or `-old`).
- `GET /labels` gives user labels a `keywordState`: `mirrored`; `local_only` (the server
  keeps no keywords or refused them: the label exists only in Silo, `account.error`
  `keywords_unsupported`); `partial` with `localOnlyFolders` (`f_` ids): a keyword
  everywhere except those folders, where it lives only in Silo (`label.changed` is
  written when a folder is added).
- Folder limit: a Dovecot maildir folder keeps 26 distinct keywords (letters a-z in the
  file names; sdbox/mdbox have no limit). Dovecot 2.4 does not refuse a 27th, it keeps it
  only in its index (lost when the index is rebuilt), so Silo counts the folder's
  `PERMANENTFLAGS` against `SILO_SYNC_KEYWORD_FOLDER_LIMIT` and does not store a label
  keyword beyond it: the label becomes `partial` for that folder. A `NO` to the `STORE`
  of a keyword in a folder that announces `\*` has the same effect. Set the limit to `0`
  for servers without it.

## Cache rebuild

`POST /accounts/{id}/resync` with `{"mode": "rebuild"}` (or `silo-server rebuild-cache
--account ID`, or `--all` for every account that is not disabled, for an administrator,
also after `TRUNCATE` of cache tables or a cache-only migration) drops what
Silo mirrors for the account and runs the initial sync from scratch. The request is
recorded in `account_rebuilds` and done by the account's worker (on the running server,
or on the next `serve`): the live session ends, the cache is wiped in one transaction,
then `account.status` (discovering, initial_sync, live), `resync` and `label.counts`
events follow.

- Wiped: messages and their locations, references, attachments rows, threads,
  `thread_labels`, sync cursors; folders stay with their sync state reset. Blobs are
  only released (`ref_count`); equal content is written to the same blob again.
- Kept: labels (names, colours, tree, keywords), filters, settings, contacts, search
  history, and the local messages of drafts and sends with their threads (re-attached
  to their server copies by `X-Silo-Draft-Id` and Message-ID).
- Carried over by `dedupe_key` (Message-ID + size) and re-attached while the messages
  come back: snoozes (wake time, source folder), assignments of labels that are local
  only (`local_only`, or in a `localOnlyFolders` folder), the message a draft answers.
  Whatever is still missing when every folder finished its header backfill is dropped.
- User labels with keywords come back from the server; without the `labels` rows
  (no metadata) they are created again, named after their keywords.
- Lost: undo of operations made before the rebuild.
- Refused with `409 rebuild_blocked` while a message is being sent or due within a
  minute (`outbound`; mail scheduled for later stays local and queued) or operations
  wait for write-back (`imap_op` jobs); try again when `sync-status.pendingOps` is 0.
  The worker checks again under the account lock before it wipes, and waits while
  something blocks. `rebuild-cache --all` goes on past a refused account and reports it.

## Account autoconfiguration

`POST /api/v1/accounts/discover {"email": "..."}` (signed in) suggests the IMAP
and SMTP settings of an address before an account is added. The answer has
`imap` / `smtp` (`host`, `port`, `tls`: `implicit` or `starttls`, never `none`),
`username` with `loginFormat` (`email` / `localpart`), `provider`
(`generic`, `gmail`, `microsoft`), the winning `source` and every source tried
in `sources` with `ok` and a reason. `params` holds the same settings under the
field names of `POST /accounts/test`: add `password` and send it. Nothing found
is still `200`, with `imap`, `smtp` and `source` null.

Sources, ranked in this order:

1. the Thunderbird ISPDB, `GET <SILO_AUTOCONFIG_ISPDB_URL><domain>`
   (default `https://autoconfig.thunderbird.net/v1.1/`; `off` disables it);
2. the domain's own autoconfig file:
   `https://autoconfig.<domain>/mail/config-v1.1.xml?emailaddress=<email>`, then
   `https://<domain>/.well-known/autoconfig/mail/config-v1.1.xml`;
3. DNS SRV records (RFC 6186): `_imaps._tcp` (implicit TLS), `_imap._tcp`
   (STARTTLS), `_submissions._tcp` (implicit), `_submission._tcp` (STARTTLS);
   lowest priority, then highest weight; a `.` target is skipped;
4. guesses, only for a protocol still missing: `imap.<domain>:993`,
   `mail.<domain>:993`, `mail.<domain>:143` (STARTTLS); `smtp.<domain>:587`,
   `mail.<domain>:587` (STARTTLS), `smtp.<domain>:465`. A guess counts only after a
   TLS handshake with a certificate valid for the name; Silo never logs in.

Sources 1 to 3 run in parallel with 3 s each; the first one (in rank order) that
has both servers cancels the worse ones. Guesses get another 3 s, the whole call
at most 8 s. In the autoconfig format POP3 servers, `socketType` `plain` and
servers without a password or OAuth2 method are skipped (with the reason in
`sources`); OAuth2 is noted there for the OAuth2 work of phase 5.

With `SILO_ALLOW_CUSTOM_SERVERS=false` in mode `imap` users can only add the
default servers, so the endpoint contacts nothing and answers at once with
`SILO_DEFAULT_*` (`source: "default"`, username after
`SILO_DEFAULT_LOGIN_FORMAT`). In mode `local` or with custom servers allowed it
runs the full discovery. It is limited to 10 calls per minute and user (429
`rate_limited`), on top of the per-IP API limit.

Anti-SSRF: every request and handshake of the discovery, ISPDB included, goes
through the same dialer as custom mail servers: the host is resolved by Silo and
only public addresses (or entries of `SILO_CUSTOM_SERVER_ALLOWLIST`) are dialled,
so `autoconfig.<domain>` pointing at an internal address is refused before any
connection. Proxies from the environment are ignored, redirects are followed
only to `https` on the same host or the ISPDB host (at most 3), and responses
are cut at 256 kB. The discovered settings still go through `POST /accounts/test`
and its checks before an account is saved.

## OAuth2 (Gmail, Microsoft 365)

Gmail and Microsoft 365 accounts can be added with "Sign in with Google /
Microsoft" instead of a password (Exchange Online has basic auth turned off).
Silo stores the OAuth2 refresh token, encrypted with `SILO_SECRET_KEY` like a
password (AAD = account id, re-encrypted by `rotate-secrets`), and logs in to
IMAP and SMTP with `AUTHENTICATE XOAUTH2` / `AUTH XOAUTH2` and a fresh access
token. The admin registers one OAuth application per provider (instructions:
`docs/admin/oauth.md` in the `silo` repository) and sets:

| Variable | Meaning |
|---|---|
| `SILO_OAUTH_GOOGLE_CLIENT_ID`, `SILO_OAUTH_GOOGLE_CLIENT_SECRET` | Google OAuth client ("Web application") |
| `SILO_OAUTH_MICROSOFT_CLIENT_ID`, `SILO_OAUTH_MICROSOFT_CLIENT_SECRET` | Entra ID app registration |
| `SILO_OAUTH_MICROSOFT_TENANT` | `common` (default), `organizations`, `consumers` or a tenant id/domain |
| `SILO_OAUTH_{GOOGLE,MICROSOFT}_{AUTH,TOKEN,USERINFO}_URL` | **test fields only**: override the official endpoints (a fake provider in tests); leave unset in production |

A provider is enabled when both its client id and secret are set (one of
them alone is a configuration error); `GET /meta` lists the enabled ones in
`oauthProviders` (`[]` when none). The redirect URI to register with the
provider is

    SILO_PUBLIC_URL + /api/v1/oauth/{google|microsoft}/callback

e.g. `https://mail.example.com/api/v1/oauth/google/callback`.

| Provider | Scopes | IMAP | SMTP | `sentAppend` |
|---|---|---|---|---|
| Google (`provider: gmail`) | `https://mail.google.com/ openid email`, `access_type=offline`, `prompt=consent` | `imap.gmail.com:993` implicit TLS | `smtp.gmail.com:465` implicit TLS | off |
| Microsoft (`provider: microsoft`) | `https://outlook.office.com/IMAP.AccessAsUser.All https://outlook.office.com/SMTP.Send offline_access openid email` | `outlook.office365.com:993` implicit TLS | `smtp.office365.com:587` STARTTLS | off |

Gmail's SMTP is used on 465 (TLS from the first byte, nothing in clear text to
strip) rather than 587; Microsoft offers only 587 + STARTTLS. Both providers
store what is sent through their SMTP in Sent themselves (plan 3.9), so
`sentAppend` is off; it can still be turned on with `PATCH /accounts/{id}`.
`prompt=consent` makes Google return a refresh token on every sign-in, also a
second one.

Flow (authorization code with PKCE S256):

1. silo-web calls `POST /api/v1/oauth/{provider}/start` (signed in) with
   `{returnTo?, accountId?}` and opens the returned `url`. Silo stores the state
   (32 random bytes), the PKCE verifier, the user and `returnTo` in
   `oauth_states` for 10 minutes and sets the `silo_oauth_state` cookie
   (HttpOnly, SameSite=Lax, path `/api/v1/oauth/`). `returnTo` must be a path
   starting with a single `/` (`400 validation_failed` otherwise); an
   unconfigured provider is `404 not_found`. The endpoints use the `auth`
   rate-limit bucket.
2. The provider sends the browser to `GET /api/v1/oauth/{provider}/callback`.
   It needs no session (the state names the user), but the state must match
   the cookie: an authorization started in another browser (a link to the
   consent page sent by someone else) cannot add the victim's mailbox to the
   sender's Silo user. The state is single use. Silo exchanges the code with the
   verifier, takes the address from the `email` claim of the `id_token` (read
   from the token endpoint over TLS, its signature is not checked; Microsoft
   without `email`: `preferred_username`) or from userinfo, tests IMAP and SMTP
   with XOAUTH2 (`POST /accounts`-style: on failure nothing is saved), creates
   the account (`authType: xoauth2`, `username` = the address, `displayName`
   from the `name` claim, within `SILO_MAX_ACCOUNTS_PER_USER`) and queues its
   initial sync. A mailbox the user already has (same server and user name) is
   updated instead: new refresh token, and a failing account reconnects.
3. The callback answers `303` to `SILO_PUBLIC_URL + returnTo` with
   `?oauth=ok&accountId=acc_…` or `?oauth=error&code=…`: `oauth_denied` (the
   user declined), `oauth_state_invalid` (unknown, used, expired or from another
   browser), `oauth_exchange_failed` (also: no refresh token), `oauth_no_email`,
   `oauth_account_mismatch`, `account_limit_reached`, `account_test_failed`,
   `internal_error`. Expired `oauth_states` are deleted by the hourly `blob_gc`.

Tokens: before every IMAP or SMTP connection Silo asks for the account's
access token; it is cached in memory per account and refreshed only when it
expires within 60 s. A refresh token that the provider rotates is stored
again. A refresh refused with `invalid_grant` (access revoked, password
changed, Google's 7-day limit for apps in "Testing") puts the account in
`auth_failed` with `error: "OAuth authorization expired; sign in again"` and
an `account.error` event with code `auth_failed`; sync stops (no retries) and
a send fails at once. Re-authorization: `POST /oauth/{provider}/start` with
`accountId` (an OAuth account of the user with that provider, else `404`); the
address signed in with must be the account's (`oauth_account_mismatch`
otherwise). The account then reconnects. OAuth accounts have no password:
`PATCH /accounts/{id}` with `password` is `409 not_available`. `Account` has
`authType` (`password` / `xoauth2`).

Manual test (entry for the compatibility matrix; there are no test accounts
in CI): with the provider configured, (1) "Sign in with Google" adds the
account and returns to silo-web with `oauth=ok`; (2) the initial sync ends in
`live`, INBOX matches the webmail; (3) archive a thread: it leaves the Inbox in
the webmail and stays in All Mail, no duplicate in Silo; (4) apply a label and
check it on the server (a keyword, see below); (5) send a message: delivered,
one copy in Sent; (6) reply from another client: the reply joins the thread
in Silo; (7) revoke the app (Google account > Security > Third-party access):
the account goes `auth_failed` on its next connection, re-authorization brings
it back. Same steps for Microsoft 365 (Exchange Online needs SMTP AUTH enabled
for the mailbox).

### Gmail in generic mode

The `provider=gmail` mode with Gmail's extensions (`X-GM-LABELS`, `X-GM-MSGID`,
`X-GM-THRID`, syncing only All Mail, Trash and Spam) is postponed to 1.1. Gmail
accounts, with OAuth or an app password, use the generic mode of plan 3.4:

- Silo syncs INBOX and the special folders (`[Gmail]/All Mail` = `\All`, Sent
  Mail, Drafts, Trash, Spam). A message in INBOX is also in All Mail; dedup
  (`Message-ID` + size) keeps one message with both locations, one thread.
- Archiving removes the INBOX copy (`\Deleted` + `UID EXPUNGE`, which is
  Gmail's "remove the Inbox label"); the copy in All Mail stays and no
  `Archive` folder is created. This applies when the account has an All Mail
  folder under `[Gmail]/` or `[Google Mail]/` and no Archive folder; other
  servers with `\All` keep archiving by MOVE to Archive. Removing a folder
  label (a Gmail label shown as a folder) likewise only expunges its copy.
- A message only in All Mail (archived in Gmail) has no `INBOX` label and is
  shown under "All mail".
- Missing without X-GM: Gmail labels other than the special folders are not
  shown as Silo labels, and threads are built by Silo (JWZ over
  `References`/`In-Reply-To`), not by Gmail's `X-GM-THRID`, so a few threads may
  be grouped differently than in the Gmail web client. Silo's own labels are
  IMAP keywords, which Gmail does not show as labels.

## Users, mode `local` and the admin panel

### Mode `local`

With `SILO_AUTH_MODE=local` users sign in with a Silo password (argon2id)
instead of their mailbox credentials. Nobody can sign up: an admin creates
users, and users add their mail accounts themselves after signing in
(`POST /accounts`, host given explicitly). `SILO_DEFAULT_IMAP_HOST` is optional
in this mode; custom servers are always allowed (`GET /meta` reports
`allowCustomServers: true`) and every host other than a configured default must
resolve to a public address or be on `SILO_CUSTOM_SERVER_ALLOWLIST`.

Sign-in failures (unknown email, wrong password, disabled user) all answer
`401 invalid_credentials` after the same argon2id work and the same minimum time;
the per-IP and per-login rate limits apply as in mode `imap`. `POST
/session/password` (`{currentPassword, newPassword}`, at least 10 characters)
changes the password and signs out the user's other sessions; in mode `imap` it
answers `409 not_available`.

Create the first admin from the command line:

```sh
silo-server user add --email admin@example.com --admin      # prints a generated password once
echo 'a long password' | silo-server user add --email ala@example.com --password-stdin
```

### `user` command

```sh
silo-server user add --email X [--name N] [--admin] [--password-stdin]
silo-server user passwd --email X        # new password from stdin (mode local), signs the user out
silo-server user admin --email X --set=true|false
silo-server user list
```

It needs `SILO_DATABASE_URL` and reads `SILO_AUTH_MODE`. In mode `imap`, `user
add` stores no password (the user signs in with the mailbox, which links the
account on first sign-in) and `user admin` is how a mailbox user becomes admin.

### TOTP (mode `local`)

Authenticator-app codes (RFC 6238: SHA1, 30 s, 6 digits, ±1 step) as a second
factor. The secret is stored encrypted with `SILO_SECRET_KEY` (and re-encrypted
by `rotate-secrets`); a code of an already used time step is refused; backup
codes are stored as SHA-256 hashes and work once. In mode `imap` every TOTP
endpoint answers `409 not_available`.

| Request | Effect |
|---|---|
| `POST /totp/setup` | new secret: `{secret, otpauthUrl}` (show as QR code); not enforced yet; `409 totp_enabled` when on |
| `POST /totp/enable {code}` | turns TOTP on, returns 10 `backupCodes` once (`422 invalid_totp_code` for a wrong code) |
| `DELETE /totp {password, code}` | turns TOTP off (`code`: TOTP or backup code; `403 invalid_credentials` when wrong) |
| `POST /totp/backup-codes {password, code}` | replaces the backup codes |

Sign-in: `POST /session` takes an optional `totpCode` (TOTP or backup code). For
a user with TOTP on, the right password without a code answers `401
totp_required` (the client asks for the code and posts again), a wrong code `401
invalid_credentials`. `GET /session` reports `user.totpEnabled`.

### Admin panel

Endpoints for users with `is_admin` (others get `403 forbidden`), in both modes:

| Request | Effect |
|---|---|
| `GET /admin/users` | users with `isAdmin`, `disabled`, `totpEnabled` and their accounts' states |
| `POST /admin/users {email, displayName?, isAdmin?, password?}` | mode `local` only; without `password` a generated `temporaryPassword` is returned once |
| `PATCH /admin/users/{id} {disabled?, isAdmin?, displayName?, password?}` | disabling signs the user out and sets their accounts to `disabled` (sync stops); enabling reconnects them; `password` only in mode `local` |
| `POST /admin/users/{id}/totp/reset` | turns the user's TOTP off (lost device) |
| `DELETE /admin/users/{id}` | removes the user and everything Silo keeps for them (mail servers untouched) |
| `GET /admin/accounts` | every account with `authType`, state, error, `pendingOps` and `deadOps` |
| `GET /admin/jobs?status=dead\|pending&accountId=` | newest 200 jobs, without payloads |
| `POST /admin/jobs/{id}/retry` | a dead job runs again (attempts reset) |
| `DELETE /admin/jobs/{id}` | discards a job that is not running |

Admins cannot disable, demote or delete themselves (`409 admin_self_change`). A
disabled user cannot sign in in either mode, and their existing sessions stop
working at once.

## Backup and restore

Postgres holds two kinds of data (plan 3.3, 4.1): the **cache** (messages,
folders, threads, system labels, mirror blobs), which the sync rebuilds from
the IMAP server, and the **metadata** that exists only in Silo. A metadata
backup is one JSON document:

```sh
silo-server export-metadata --out silo-metadata.json   # all users; --user EMAIL for one
silo-server migrate up                                  # on the new database
silo-server import-metadata --in silo-metadata.json     # then start serve
```

What is in it: users (password hash, settings, TOTP), accounts (server
configuration, the sealed mailbox password; no sync state), user labels
(name, keyword, colour, nesting), user-label assignments and snoozes keyed by
Message-ID, drafts and messages waiting to be sent (with their attachments
and stored MIME inline as base64; `--no-local-blobs` keeps only the sha256),
filters (name, criteria, actions, enabled, order; the labels a filter adds
are written by name in `actions.addLabels` and mapped to the imported labels,
a name without a label is dropped with a warning), manual contacts and search
history. Not in it: sessions, events,
jobs, write-back operations and the cache. Rows are keyed by stable identities
(user email; account = user + IMAP host + username; label = account + name;
message = Message-ID), never by local ids. The format is versioned and
extensible: unknown sections are skipped with a warning, missing ones are
empty.

- **Same `SILO_SECRET_KEY`.** Mailbox passwords and TOTP secrets stay
  encrypted with `SILO_SECRET_KEY` (re-sealed for the export, bound to the
  stable keys instead of row ids). The import needs the same key (or it as
  `SILO_SECRET_KEY_PREVIOUS`); keep the key with the backup, separately.
- **Import** goes into a migrated database and fails if a user exists
  already; `--merge` imports into existing users instead (accounts found by key
  get the exported configuration, labels are upserted by name, assignments,
  snoozes, filters, contacts and history are added, drafts already present are
  skipped: importing twice changes nothing). Imported accounts start an
  initial sync. Label assignments and snoozes wait in `metadata_restores` until
  the sync has ingested their messages: the `metadata_restore` job applies them
  as messages arrive (mirrored labels also get their keyword on the server),
  retries every minute and gives up after 24 h (the job is then `dead` in the
  admin job list and the unmatched rows stay with `last_error`). Scheduled
  messages are queued again for their time (at once if it has passed).
- **Limits.** Write-back operations still pending (`imap_op`, e.g. an archive
  inside the undo window) are not in the backup: they run within seconds,
  `backup.sh` waits until the account queue is empty. A draft opened from another
  client's Drafts copy (no `X-Silo-Draft-Id`) comes back as a local draft next
  to that copy. A snooze already due is restored as is (woken by the snooze job).

## S3Store

`SILO_BLOB_BACKEND=s3` keeps blobs in an S3-compatible bucket (MinIO, AWS S3,
...) as `<prefix>/ab/cd/<sha256>`, content-addressed like the filesystem store.

| Variable | Default | |
|---|---|---|
| `SILO_BLOB_S3_ENDPOINT` | – (required) | `host:port`, or a URL (`https://...`; the scheme wins over `_USE_SSL`) |
| `SILO_BLOB_S3_BUCKET` | – (required) | must exist: `serve` checks it at startup and does not create it |
| `SILO_BLOB_S3_ACCESS_KEY` / `_SECRET_KEY` | – (required) | credentials |
| `SILO_BLOB_S3_REGION` | `us-east-1` | |
| `SILO_BLOB_S3_PREFIX` | empty | key prefix, e.g. `silo/blobs` |
| `SILO_BLOB_S3_USE_SSL` | `true` | |
| `SILO_BLOB_S3_PATH_STYLE` | `true` | path-style bucket addressing (MinIO); `false` for virtual-hosted style |

`Put` spools the upload to a temporary file to hash it first (the key is the
hash), so the server needs temporary disk space for the largest blob.
Switching backends does not move existing blobs.

## Development

- **Schema**: `migrations/` (golang-migrate, embedded). Queries live in
  `internal/store/queries/*.sql`; every query touching a user-owned table must
  filter on `user_id` (checked by a test). After changing either, run
  `make sqlc` and commit `internal/store/sqlcgen`.
- **API contract**: huma operations in `internal/http/api`. After changing
  them, run `make openapi` and commit `api/openapi.yaml`.
- **Tests**: `go test ./...` runs integration tests against a throwaway
  Postgres (testcontainers). They are skipped with `-short` or when Docker is
  unavailable; set `SILO_REQUIRE_DOCKER=true` to make that a failure (CI does).
  `make test` uses `-race`, which needs a C compiler.

### Migrations and background jobs

Migrations of Faza 4 (all additive: new columns, tables and indexes):

| Migration | Adds |
|---|---|
| `000006_search_index` | `messages.search_ver` (tokenizer version) and partial indexes for search; `search_tsv` is rebuilt by the application tokenizer — rows below the current version are re-indexed **in the background at start-up** (`mail.ReindexSearch`, batches of 500), so search over older mail is incomplete until it finishes |
| `000007_filters` | `filters.name`, `enabled`, `updated_at` and an index per user |
| `000008_thumbnails` | table `thumbnails` (cache of attachment thumbnails per source blob and size) |
| `000009_label_folders_rebuild` | `label_folder_state` (per-folder keyword state, `keywordState: partial`) and `account_rebuilds` (cache rebuild of an account) |

Migrations of Faza 5 (numbered from `000050`, after the Faza 4 range; additive):

| Migration | Adds |
|---|---|
| `000050_local_auth_totp` | `users` columns of mode `local` and TOTP (`totp_secret_enc`, `totp_enabled_at`, `totp_last_step`, `totp_backup_codes`, `disabled_at`, `password_changed_at`) |
| `000051_metadata_restores` | `metadata_restores` (label assignments and snoozes of an `import-metadata` waiting for their messages) |
| `000052_oauth` | `oauth_states` (OAuth2 sign-in in progress, 10 min) |

General jobs (table `jobs`, run by the worker of `serve`; per-account sync work and the
cache rebuild run in the sync engine):

| Job | Queued by | Does |
|---|---|---|
| `imap_op` | label/move/flag operations, folders, drafts | writes changes back to the mail server (after the undo window for folder moves) |
| `send` | `POST /drafts/{id}/send` | SMTP send at `sendAt` (undo-send window or schedule), then Sent append |
| `blob_gc` | hourly | drafts GC, unreferenced blobs and thumbnails, interrupted sends, old operations and expired `oauth_states` |
| `snooze_wake` | every 30 s and ingest | wakes due snoozes, drops snoozes taken out of Snoozed elsewhere |
| `filter_eval` | ingest (new INBOX mail of accounts with filters) | runs the enabled filters on new messages |
| `filter_apply` | `POST /filters/{id}/apply` | applies a filter to existing mail, then `filter.applied` |
| `metadata_restore` | `import-metadata` | applies staged label assignments and snoozes as the sync ingests their messages (every minute, gives up after 24 h) |

The search re-index (`search_ver`) is not a job: `serve` runs it once in the
background at start-up until no row has an older tokenizer version.

### IMAP integration tests

`internal/imapx` and the future sync engine are tested against a real Dovecot
(`dovecot/dovecot:2.4.5`, pinned) started per test by `internal/testutil/imaptest`
(`imaptest.Start(t, profile)` returns address, user and password, `StartServer` also
gives access to the container; plaintext IMAP, account `silo`). Profiles select what
the server advertises:

| Profile | Advertises | Used for |
|---|---|---|
| `full` | Dovecot defaults: `CONDSTORE QRESYNC MOVE UIDPLUS IDLE SPECIAL-USE ...` | main sync path |
| `no-condstore` | everything except `CONDSTORE QRESYNC` | full-scan fallback |
| `no-move` | everything except `MOVE UIDPLUS` (and `IMAP4rev2`) | COPY + STORE + EXPUNGE fallback |
| `gmail-like` | Dovecot defaults, Gmail's folders (`[Gmail]/All Mail` = `\All`, ...) | Gmail in generic mode (`internal/sync`) |
| `oauth2` | Dovecot defaults plus `AUTH=XOAUTH2`; tokens checked at the fake provider of `testutil/oauthtest` (`imaptest.StartOAuth2`) | XOAUTH2 (`internal/imapx`, `internal/http/api`) |

Large mailbox (DoD of Faza 5: 200k messages, RAM < 512 MB): `TestInitialSync` takes
its size from `SILO_SYNC_TEST_MESSAGES` (default 2000) and samples the memory of the
test process every 2 s while the initial sync runs (Go `Sys`/`HeapInuse` and the
resident set: `GetProcessMemoryInfo` on Windows, `/proc/self/status` on Linux).
`SILO_SYNC_TEST_REPORT=<file>` writes the peaks, timings and the count of WARN/ERROR
log records as JSON; `SILO_SYNC_TEST_PPROF=<file>` keeps a heap profile at the
`HeapInuse` peak. The process also holds the test's IMAP client and Postgres pool, so
its RSS is an upper bound for `silo-server` (Dovecot and Postgres run in containers).
Above 20000 messages the mailbox is written as Maildir files into the container
(`imaptest.StartServer`, `CopyTar`) instead of `APPEND`, which slows down with the
size of a Maildir folder.

```sh
SILO_SYNC_TEST_MESSAGES=200000 SILO_SYNC_TEST_REPORT=report.json SILO_REQUIRE_DOCKER=true \
  go test ./internal/sync -run '^TestInitialSync$' -count=1 -v -timeout 3h
```

Config files and Dovecot 2.4 notes: `internal/testutil/imaptest/testdata/imap/README.md`.
Same rules as Postgres tests: skipped with `-short` or without Docker, a failure
with `SILO_REQUIRE_DOCKER=true`. `internal/imapx/spike_test.go` records what
go-imap/v2 can and cannot do; run it with `-v` to see the findings.
