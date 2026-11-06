#!/usr/bin/env bash
# Upgrade test (plan 7, phase 5): the previous release, seeded with data, is
# replaced by the current tree, which must migrate the database and serve the
# same data. Runs in CI (.github/workflows/upgrade.yml) and locally (Linux,
# macOS, Git Bash on Windows) with Docker, git, curl and Go.
#
#   PREVIOUS_TAG=v0.3.0 scripts/upgrade-test.sh
#
# Steps:
#   a. build the prod image of PREVIOUS_TAG (default: the last v* tag
#      reachable from main) from `git archive` -> silo-upg-old
#   b. build the prod image of the working tree -> silo-upg-new
#   c. start Postgres 17 (silo-upg-postgres) and Dovecot (silo-upg-dovecot,
#      the test configuration of internal/testutil/imaptest, profile full)
#   d. old image: migrate up, serve; sign in, deliver 4 messages, wait for the
#      sync, label a thread; from v0.4.0 also create a filter and snooze a
#      thread for 1 h; save the listings (internal/testutil/upgradecheck)
#   e. new image: migrate up (the version must grow), serve, sign in with the
#      same password, same INBOX with the label, the thread still SNOOZED,
#      the filter still there, reconcile without drift,
#      /meta reports the new version
#   f. everything silo-upg-* is removed on exit (KEEP=1 keeps it for debugging)
#
# Environment: PREVIOUS_TAG, NEW_VERSION (default: git describe), SILO_UPG_PG_PORT
# and SILO_UPG_HTTP_PORT (host ports; random when unset), KEEP=1.
set -euo pipefail

# Git Bash: do not rewrite "container:/path" arguments of docker into Windows paths.
export MSYS_NO_PATHCONV=1

cd "$(git rev-parse --show-toplevel)"
start=$(date +%s)

PREVIOUS_TAG=${PREVIOUS_TAG:-$(git describe --tags --abbrev=0 --match 'v*' main 2>/dev/null || git describe --tags --abbrev=0 --match 'v*')}
NEW_VERSION=${NEW_VERSION:-$(git describe --tags --always --match 'v*')-upgrade}
P=silo-upg
NET=$P-net
PG=$P-postgres
IMAP=$P-dovecot
SRV=$P-server
VOL=$P-blobs
OLD_IMG=$P-old
NEW_IMG=$P-new
IMAP_IMAGE=dovecot/dovecot:2.4.5
PG_IMAGE=postgres:17
TMP=$(mktemp -d)

log() { printf '\n== %s (%ss)\n' "$*" "$(($(date +%s) - start))"; }

# native turns a path into one the (possibly Windows) docker and go binaries read.
native() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }

cleanup() {
	status=$?
	if [ "$status" -ne 0 ] && docker inspect "$SRV" >/dev/null 2>&1; then
		echo "--- last server log lines ---"
		docker logs --tail 60 "$SRV" 2>&1 || true
	fi
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: leaving $P-* containers, volume and images"
	else
		docker rm -f "$SRV" "$PG" "$IMAP" >/dev/null 2>&1 || true
		docker volume rm "$VOL" >/dev/null 2>&1 || true
		docker network rm "$NET" >/dev/null 2>&1 || true
		docker image rm "$OLD_IMG" "$NEW_IMG" >/dev/null 2>&1 || true
	fi
	rm -rf "$TMP"
	if [ "$status" -eq 0 ]; then
		echo "upgrade test PASSED: $PREVIOUS_TAG -> $NEW_VERSION in $(($(date +%s) - start))s"
	else
		echo "upgrade test FAILED (exit $status) after $(($(date +%s) - start))s" >&2
	fi
	exit "$status"
}
trap cleanup EXIT

# hostport prints the host address of a published container port.
hostport() { docker port "$1" "$2" | head -n1 | sed 's/^0\.0\.0\.0:/127.0.0.1:/'; }

publish() { if [ -n "${1:-}" ]; then printf '127.0.0.1:%s:%s' "$1" "$2"; else printf '127.0.0.1::%s' "$2"; fi; }

# Settings both releases accept (no variable added since PREVIOUS_TAG is needed).
SECRET=$(head -c 32 /dev/urandom | base64 | tr -d '\n')
ENV=(
	-e SILO_DATABASE_URL="postgres://silo:silo@$PG:5432/silo?sslmode=disable"
	-e SILO_PUBLIC_URL=http://localhost:8080
	-e SILO_SECRET_KEY="$SECRET"
	-e SILO_LOG_FORMAT=text
	-e SILO_AUTH_MODE=imap
	-e SILO_DEFAULT_IMAP_HOST="$IMAP"
	-e SILO_DEFAULT_IMAP_PORT=31143
	-e SILO_DEFAULT_IMAP_TLS=none
	-e SILO_DEFAULT_SMTP_PORT=25
	-e SILO_DEFAULT_SMTP_TLS=none
	-e SILO_ALLOW_INSECURE_TRANSPORT=true
	-e SILO_DEFAULT_LOGIN_FORMAT=localpart
	-e SILO_DEFAULT_DOMAIN=silo.test
	-e SILO_BLOB_FS_DIR=/data/blobs
	-e SILO_SYNC_POLL_INTERVAL=5s
	-e SILO_UNDO_WINDOW=1s
)

run() { docker run --rm --network "$NET" "${ENV[@]}" "$@"; }

serve() {
	docker rm -f "$SRV" >/dev/null 2>&1 || true
	docker run -d --name "$SRV" --network "$NET" -v "$VOL:/data" -p "$(publish "${SILO_UPG_HTTP_PORT:-}" 8080)" "${ENV[@]}" "$1" serve >/dev/null
	BASE="http://$(hostport "$SRV" 8080/tcp)"
	for _ in $(seq 1 60); do
		if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then
			echo "$1 serving at $BASE"
			return 0
		fi
		if [ "$(docker inspect -f '{{.State.Running}}' "$SRV")" != true ]; then
			echo "$1 exited" >&2
			return 1
		fi
		sleep 1
	done
	echo "$1: /healthz not ready" >&2
	return 1
}

migrate_version() { run "$1" migrate version | sed -n 's/^version=\([0-9]*\).*/\1/p'; }

for tool in docker git curl go; do
	command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 1; }
done
git rev-parse -q --verify "refs/tags/$PREVIOUS_TAG" >/dev/null || { echo "unknown tag $PREVIOUS_TAG" >&2; exit 1; }
echo "upgrade test: $PREVIOUS_TAG -> working tree ($NEW_VERSION)"
docker rm -f "$SRV" "$PG" "$IMAP" >/dev/null 2>&1 || true

log "a. build $PREVIOUS_TAG ($OLD_IMG)"
git archive --format=tar "$PREVIOUS_TAG" | docker build -q --target prod --build-arg VERSION="$PREVIOUS_TAG" -t "$OLD_IMG" - >/dev/null

log "b. build the working tree ($NEW_IMG)"
docker build -q --target prod --build-arg VERSION="$NEW_VERSION" -t "$NEW_IMG" . >/dev/null

log "c. start Postgres and Dovecot"
docker network create "$NET" >/dev/null
docker run -d --name "$PG" --network "$NET" -p "$(publish "${SILO_UPG_PG_PORT:-}" 5432)" \
	-e POSTGRES_USER=silo -e POSTGRES_PASSWORD=silo -e POSTGRES_DB=silo "$PG_IMAGE" >/dev/null
docker create --name "$IMAP" --network "$NET" -p 127.0.0.1::31143 "$IMAP_IMAGE" >/dev/null
conf=internal/testutil/imaptest/testdata/imap
docker cp "$(native "$conf/dovecot.conf")" "$IMAP:/etc/dovecot/dovecot.conf"
docker cp "$(native "$conf/users")" "$IMAP:/etc/dovecot/users"
docker cp "$(native "$conf/profile-full.conf")" "$IMAP:/etc/dovecot/profile.conf"
docker start "$IMAP" >/dev/null
# The prod image runs as nonroot (65532): give it the blob volume.
docker volume create "$VOL" >/dev/null
docker run --rm -v "$VOL:/data" --entrypoint sh "$PG_IMAGE" -c 'mkdir -p /data/blobs && chown -R 65532:65532 /data'
for _ in $(seq 1 60); do
	docker exec "$PG" pg_isready -q -U silo -d silo && break
	sleep 1
done
docker exec "$PG" pg_isready -q -U silo -d silo
echo "postgres at $(hostport "$PG" 5432/tcp), dovecot at $(hostport "$IMAP" 31143/tcp)"

STATE=$(native "$TMP/state.json")
CHECK="$TMP/upgradecheck$(go env GOEXE)"
go build -o "$(native "$CHECK")" ./internal/testutil/upgradecheck

log "d. $PREVIOUS_TAG: migrate, serve, seed"
run "$OLD_IMG" migrate up
OLD_SCHEMA=$(migrate_version "$OLD_IMG")
echo "schema version $OLD_SCHEMA"
serve "$OLD_IMG"
"$CHECK" seed -base "$BASE" -imap "$(hostport "$IMAP" 31143/tcp)" -state "$STATE"
docker stop -t 30 "$SRV" >/dev/null

log "e. $NEW_VERSION: migrate, serve, verify"
run "$NEW_IMG" migrate up
NEW_SCHEMA=$(migrate_version "$NEW_IMG")
echo "schema version $OLD_SCHEMA -> $NEW_SCHEMA"
if [ -z "$NEW_SCHEMA" ] || [ "$NEW_SCHEMA" -le "$OLD_SCHEMA" ]; then
	echo "schema version did not grow" >&2
	exit 1
fi
serve "$NEW_IMG"
"$CHECK" verify -base "$BASE" -state "$STATE" -expect-version "$NEW_VERSION"
run "$NEW_IMG" reconcile
echo "reconcile: no drift"

log "f. clean up"
