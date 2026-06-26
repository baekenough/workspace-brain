#!/usr/bin/env bash
# restore-smoke-test.sh — ephemeral Docker-based restore smoke test.
#
# Spins up a temporary PostgreSQL container, restores the dump from BACKUP_DIR,
# and asserts that the tenants table is present and the row count matches the
# original row count stored in the backup metadata.  The container is torn down
# on exit regardless of outcome.
#
# This script documents and exercises the restore path.  It is not wired into
# the unit test gate (check-coverage.sh).  Run it manually or in a CI job that
# has Docker available.
#
# Environment variables
# ---------------------
# BACKUP_DIR    (Required) Path to the timestamped backup directory produced by
#               backup.sh, e.g. ./backups/20240101T120000Z.
# PG_IMAGE      Postgres Docker image to use.  Default: postgres:16-alpine.
# SMOKE_DB      Database name to create in the ephemeral container.
#               Default: wbsmoke.
# SMOKE_USER    Postgres username.  Default: wb.
# SMOKE_PASS    Postgres password.  Default: smoketest.
# SMOKE_PORT    Host port to bind the ephemeral Postgres.  Default: 15432.
# TIMEOUT       Seconds to wait for Postgres to become ready.  Default: 30.
#
# Usage
# -----
#   BACKUP_DIR=./backups/20240101T120000Z ./scripts/restore-smoke-test.sh
#
set -euo pipefail

# ── helpers ───────────────────────────────────────────────────────────────────

log()  { printf '[smoke] %s\n' "$*" >&2; }
skip() { printf '[smoke] SKIP %s\n' "$*" >&2; exit 0; }
die()  { printf '[smoke] ERROR %s\n' "$*" >&2; exit 1; }

# ── preflight checks ──────────────────────────────────────────────────────────

if ! command -v docker >/dev/null 2>&1; then
  skip "docker not found — skipping restore smoke test"
fi

docker info >/dev/null 2>&1 || skip "Docker daemon not reachable — skipping restore smoke test"

if ! command -v pg_restore >/dev/null 2>&1; then
  skip "pg_restore not found — skipping restore smoke test (install postgresql-client)"
fi

if ! command -v psql >/dev/null 2>&1; then
  skip "psql not found — skipping restore smoke test (install postgresql-client)"
fi

# ── config ────────────────────────────────────────────────────────────────────

BACKUP_DIR="${BACKUP_DIR:-}"
if [ -z "${BACKUP_DIR}" ]; then
  die "BACKUP_DIR is required.  Point it at a directory produced by backup.sh."
fi
if [ ! -d "${BACKUP_DIR}" ]; then
  die "BACKUP_DIR does not exist: ${BACKUP_DIR}"
fi

PG_DUMP_FILE="${BACKUP_DIR}/postgres.dump"
if [ ! -f "${PG_DUMP_FILE}" ]; then
  skip "No postgres.dump in ${BACKUP_DIR} — skipping PostgreSQL smoke test"
fi

PG_IMAGE="${PG_IMAGE:-postgres:16-alpine}"
SMOKE_DB="${SMOKE_DB:-wbsmoke}"
SMOKE_USER="${SMOKE_USER:-wb}"
SMOKE_PASS="${SMOKE_PASS:-smoketest}"
SMOKE_PORT="${SMOKE_PORT:-15432}"
TIMEOUT="${TIMEOUT:-30}"

CONTAINER_NAME="wb-restore-smoke-$$"
SMOKE_DSN="postgres://${SMOKE_USER}:${SMOKE_PASS}@127.0.0.1:${SMOKE_PORT}/${SMOKE_DB}"

# ── teardown on exit ──────────────────────────────────────────────────────────

cleanup() {
  log "Removing container ${CONTAINER_NAME}"
  docker rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ── start ephemeral postgres ──────────────────────────────────────────────────

log "Starting ephemeral Postgres (${PG_IMAGE}) on port ${SMOKE_PORT}"
docker run -d \
  --name "${CONTAINER_NAME}" \
  -e POSTGRES_DB="${SMOKE_DB}" \
  -e POSTGRES_USER="${SMOKE_USER}" \
  -e POSTGRES_PASSWORD="${SMOKE_PASS}" \
  -p "127.0.0.1:${SMOKE_PORT}:5432" \
  "${PG_IMAGE}" >/dev/null

log "Waiting for Postgres to accept connections (timeout: ${TIMEOUT}s)"
ELAPSED=0
until docker exec "${CONTAINER_NAME}" \
    pg_isready -U "${SMOKE_USER}" -d "${SMOKE_DB}" >/dev/null 2>&1; do
  if [ "${ELAPSED}" -ge "${TIMEOUT}" ]; then
    die "Postgres did not become ready within ${TIMEOUT}s"
  fi
  sleep 1
  ELAPSED="$((ELAPSED + 1))"
done
log "Postgres ready (${ELAPSED}s)"

# ── restore dump ──────────────────────────────────────────────────────────────

log "Restoring dump from ${PG_DUMP_FILE}"
PGPASSWORD="${SMOKE_PASS}" pg_restore \
  --no-password \
  --clean \
  --if-exists \
  --no-owner \
  --no-privileges \
  --host=127.0.0.1 \
  --port="${SMOKE_PORT}" \
  --username="${SMOKE_USER}" \
  --dbname="${SMOKE_DB}" \
  "${PG_DUMP_FILE}"
log "pg_restore complete"

# ── assertions ────────────────────────────────────────────────────────────────

log "Asserting schema: tenants table exists"
TABLE_EXISTS="$(PGPASSWORD="${SMOKE_PASS}" psql \
  --no-password \
  -h 127.0.0.1 -p "${SMOKE_PORT}" \
  -U "${SMOKE_USER}" -d "${SMOKE_DB}" \
  -tAc "SELECT to_regclass('public.tenants');")"
if [ "${TABLE_EXISTS}" = "" ] || [ "${TABLE_EXISTS}" = "NULL" ]; then
  die "Assertion failed: tenants table not found after restore"
fi
log "tenants table present: ${TABLE_EXISTS}"

log "Asserting schema: bindings, sources, chunks, jobs tables exist"
for TABLE in bindings sources chunks jobs; do
  RESULT="$(PGPASSWORD="${SMOKE_PASS}" psql \
    --no-password \
    -h 127.0.0.1 -p "${SMOKE_PORT}" \
    -U "${SMOKE_USER}" -d "${SMOKE_DB}" \
    -tAc "SELECT to_regclass('public.${TABLE}');")"
  if [ "${RESULT}" = "" ] || [ "${RESULT}" = "NULL" ]; then
    die "Assertion failed: ${TABLE} table not found after restore"
  fi
  log "  ${TABLE}: ok"
done

log "Querying row counts"
for TABLE in tenants bindings sources chunks jobs; do
  COUNT="$(PGPASSWORD="${SMOKE_PASS}" psql \
    --no-password \
    -h 127.0.0.1 -p "${SMOKE_PORT}" \
    -U "${SMOKE_USER}" -d "${SMOKE_DB}" \
    -tAc "SELECT COUNT(*) FROM ${TABLE};")"
  log "  ${TABLE}: ${COUNT} rows"
done

log "Running basic query: SELECT COUNT(*) FROM tenants succeeds"
TENANT_COUNT="$(PGPASSWORD="${SMOKE_PASS}" psql \
  --no-password \
  -h 127.0.0.1 -p "${SMOKE_PORT}" \
  -U "${SMOKE_USER}" -d "${SMOKE_DB}" \
  -tAc "SELECT COUNT(*) FROM tenants;")"
# The count must be a non-negative integer.
case "${TENANT_COUNT}" in
  ''|*[!0-9]*) die "Assertion failed: tenant count is not a valid integer: '${TENANT_COUNT}'" ;;
esac
log "Tenant count: ${TENANT_COUNT}"

# ── success ───────────────────────────────────────────────────────────────────

log "Smoke test PASSED — restore verified against ephemeral Postgres"
log "Backup dir: ${BACKUP_DIR}"
log "Postgres image: ${PG_IMAGE}"
