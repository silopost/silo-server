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
- Sessions and accounts (`internal/auth`, `internal/accounts`): `POST/GET/DELETE /session`, `GET /sessions`, `DELETE /sessions/{id}`, `GET/POST /accounts`, `POST /accounts/test`, `GET/PATCH/DELETE /accounts/{id}`, `GET /accounts/{id}/folders`, `POST /accounts/{id}/resync`, `GET /accounts/{id}/sync-status`, `GET /meta`. The full list is `api/openapi.yaml`.
- Reading the mailbox (`internal/mailview`): `GET /labels` (counters, pseudo label
  `UNREAD`), `POST /labels` (user label with an IMAP keyword, or a folder: `CREATE`
  queued as an `imap_op` job), `PATCH`/`DELETE /labels/{id}` (a rename moves the
  keyword on the server, a folder is deleted only when empty), `GET /threads`
  (keyset pagination, `labelId`, plain full-text `q`), `GET /threads/{id}`,
  `GET /messages/{id}/html` (sanitised, immutable, CSP), `/messages/{id}/raw` and
  `GET /attachments/{id}`. A body that is still `headers`/`deferred` is downloaded
  on demand (up to 10 s, then `503 content_not_available` + `Retry-After`).
- Events: `GET /events` is a Server-Sent Events stream (24 h replay with
  `Last-Event-ID`, `: ping` every 25 s, `resync` when the id is too old), and
  `GET /events/poll?after=` the polling fallback. Rows of the `events` table are
  announced by `NOTIFY silo_events` at commit; `X-Silo-Client` is echoed as `origin`.
- Benchmark (nightly workflow): `SILO_BENCH_INBOX=true go test ./internal/mailview
  -run TestInboxListBenchmark -v` loads 100k threads / 300k messages and checks
  p95 < 50 ms and the `thread_labels_list` plan of the inbox query.
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
