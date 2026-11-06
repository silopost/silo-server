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
silo-server rebuild-cache --account ID [--yes] # drop the cached mail of an account, sync it again
silo-server version
```

`migrate` needs only `SILO_DATABASE_URL`. Migrations run under a Postgres
advisory lock, so several instances can start at once.

`rotate-secrets` rotates the key protecting stored mailbox passwords: set
`SILO_SECRET_KEY` to the new key and `SILO_SECRET_KEY_PREVIOUS` to the old one,
run the command (it needs both plus `SILO_DATABASE_URL`, is safe to repeat and
reports how many accounts were re-encrypted), then drop the previous key.

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

## Configuration

All settings are `SILO_*` environment variables; the full table (names,
defaults, meaning) is in section 2.5 of the architecture plan. `serve` checks
the whole configuration at startup and reports every problem at once. Sizes
accept `50MB` style values, rates `5MB/s`, durations `5m`. Worth knowing:

- Required for `serve`: `SILO_DATABASE_URL`, `SILO_PUBLIC_URL`, `SILO_SECRET_KEY`
  (32 random bytes, base64) and, in `SILO_AUTH_MODE=imap` (default),
  `SILO_DEFAULT_IMAP_HOST`.
- `*_TLS=none` is only accepted with `SILO_ALLOW_INSECURE_TRANSPORT=true`.
- `SILO_BLOB_BACKEND=s3` is not supported yet; `fs` stores under `SILO_BLOB_FS_DIR`.
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
- Sessions and accounts (`internal/auth`, `internal/accounts`): `POST/GET/DELETE /session`, `GET /sessions`, `DELETE /sessions/{id}`, `GET/POST /accounts`, `POST /accounts/test`, `GET/PATCH/DELETE /accounts/{id}`, `GET /accounts/{id}/folders`, `POST /accounts/{id}/resync`, `GET /accounts/{id}/sync-status`, `GET /meta`. Accounts carry `position` and are listed in that order (`GET /session`, `GET /accounts`). Migration `000005` turns `sent_append` on for generic accounts created before sending existed. The full list is `api/openapi.yaml`.
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
- Benchmark (nightly workflow): `SILO_BENCH_INBOX=true go test ./internal/mailview
  -run TestInboxListBenchmark -v` loads 100k threads / 300k messages and checks
  p95 < 50 ms and the `thread_labels_list` plan of the inbox query.
  `SILO_BENCH_SEARCH=true go test ./internal/mailview -run TestSearchBenchmark -v` loads
  1M messages (`SILO_BENCH_MESSAGES`) and checks search p95 < 500 ms for one word and `from:`.
- Sessions: the `silo_session` cookie is `Secure` when `SILO_PUBLIC_URL` is `https://`
  (plain `http://` is for local development only). Mutating requests need the
  `X-Silo-CSRF` header and an `Origin` equal to `SILO_PUBLIC_URL` or the served host.
  Sign-in rate limits and the constant answer time are built in (per IP and per login).
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
- Logging: `SILO_LOG_FORMAT` (`json`/`text`), `SILO_LOG_LEVEL`; listen address: `SILO_HTTP_ADDR`.

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
--account ID`, for an administrator, also after `TRUNCATE` of cache tables) drops what
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
- Refused with `409 rebuild_blocked` while a message is queued or being sent
  (`outbound`) or operations wait for write-back (`imap_op` jobs); try again when
  `sync-status.pendingOps` is 0.

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

General jobs (table `jobs`, run by the worker of `serve`; per-account sync work and the
cache rebuild run in the sync engine):

| Job | Queued by | Does |
|---|---|---|
| `imap_op` | label/move/flag operations, folders, drafts | writes changes back to the mail server (after the undo window for folder moves) |
| `send` | `POST /drafts/{id}/send` | SMTP send at `sendAt` (undo-send window or schedule), then Sent append |
| `blob_gc` | hourly | drafts GC, unreferenced blobs and thumbnails, interrupted sends, old operations |
| `snooze_wake` | every 30 s and ingest | wakes due snoozes, drops snoozes taken out of Snoozed elsewhere |
| `filter_eval` | ingest (new INBOX mail of accounts with filters) | runs the enabled filters on new messages |
| `filter_apply` | `POST /filters/{id}/apply` | applies a filter to existing mail, then `filter.applied` |

The search re-index (`search_ver`) is not a job: `serve` runs it once in the
background at start-up until no row has an older tokenizer version.

### IMAP integration tests

`internal/imapx` and the future sync engine are tested against a real Dovecot
(`dovecot/dovecot:2.4.5`, pinned) started per test by `internal/testutil/imaptest`
(`imaptest.Start(t, profile)` returns address, user and password; plaintext IMAP,
account `silo`). Profiles select what the server advertises:

| Profile | Advertises | Used for |
|---|---|---|
| `full` | Dovecot defaults: `CONDSTORE QRESYNC MOVE UIDPLUS IDLE SPECIAL-USE ...` | main sync path |
| `no-condstore` | everything except `CONDSTORE QRESYNC` | full-scan fallback |
| `no-move` | everything except `MOVE UIDPLUS` (and `IMAP4rev2`) | COPY + STORE + EXPUNGE fallback |

Config files and Dovecot 2.4 notes: `internal/testutil/imaptest/testdata/imap/README.md`.
Same rules as Postgres tests: skipped with `-short` or without Docker, a failure
with `SILO_REQUIRE_DOCKER=true`. `internal/imapx/spike_test.go` records what
go-imap/v2 can and cannot do; run it with `-v` to see the findings.
