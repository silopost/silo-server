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
  (up to 7 days) while the account is offline.
- Sessions: the `silo_session` cookie is `Secure` when `SILO_PUBLIC_URL` is `https://`
  (plain `http://` is for local development only). Mutating requests need the
  `X-Silo-CSRF` header and an `Origin` equal to `SILO_PUBLIC_URL` or the served host.
  Sign-in rate limits and the constant answer time are built in (per IP and per login).
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
