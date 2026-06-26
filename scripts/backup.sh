#!/usr/bin/env bash
# backup.sh — back up workspace-brain durable stores.
#
# Backs up each configured store in turn and writes timestamped artifacts to
# BACKUP_ROOT.  Stores that are not configured are skipped with an info message.
# Any error in a configured store causes an immediate non-zero exit.
#
# Environment variables
# ---------------------
# BACKUP_ROOT   Directory in which to create the timestamped backup subdirectory.
#               Default: ./backups
# DATA_PATH     Path to the workspace-brain data directory.  When set the script
#               copies $DATA_PATH/memory.json into the backup.
# PGDSN         PostgreSQL connection string (postgres://user:pass@host:port/db).
#               When set the script runs pg_dump.
# QDRANT_URL    Qdrant base URL, e.g. http://localhost:6333.
#               When set the script creates and downloads per-collection snapshots
#               via the Qdrant REST API.
#
# Usage
# -----
#   PGDSN=postgres://wb:secret@localhost:5432/wbdb \
#   DATA_PATH=/var/lib/workspace-brain \
#   QDRANT_URL=http://localhost:6333 \
#   ./scripts/backup.sh
#
set -euo pipefail

# ── helpers ───────────────────────────────────────────────────────────────────

log()  { printf '[backup] %s\n' "$*" >&2; }
skip() { printf '[backup] SKIP %s\n' "$*" >&2; }
die()  { printf '[backup] ERROR %s\n' "$*" >&2; exit 1; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

# ── setup ─────────────────────────────────────────────────────────────────────

BACKUP_ROOT="${BACKUP_ROOT:-./backups}"
TIMESTAMP="$(date -u '+%Y%m%dT%H%M%SZ')"
BACKUP_DIR="${BACKUP_ROOT}/${TIMESTAMP}"

mkdir -p "${BACKUP_DIR}"
log "Backup directory: ${BACKUP_DIR}"

# ── JSON memory snapshot ───────────────────────────────────────────────────────

if [ -z "${DATA_PATH:-}" ]; then
  skip "DATA_PATH not set — skipping memory.json backup"
else
  SNAPSHOT="${DATA_PATH}/memory.json"
  if [ ! -f "${SNAPSHOT}" ]; then
    skip "DATA_PATH set but ${SNAPSHOT} does not exist — skipping"
  else
    log "Backing up memory snapshot: ${SNAPSHOT}"
    cp "${SNAPSHOT}" "${BACKUP_DIR}/memory.json"
    log "memory.json saved"
  fi
fi

# ── PostgreSQL ─────────────────────────────────────────────────────────────────

if [ -z "${PGDSN:-}" ]; then
  skip "PGDSN not set — skipping PostgreSQL backup"
else
  require_cmd pg_dump
  PG_DUMP_FILE="${BACKUP_DIR}/postgres.dump"
  log "Running pg_dump → ${PG_DUMP_FILE}"
  # Custom format: compressed, supports parallel pg_restore.
  pg_dump --format=custom --no-password --file="${PG_DUMP_FILE}" "${PGDSN}"
  log "pg_dump complete ($(du -sh "${PG_DUMP_FILE}" | cut -f1))"
fi

# ── Qdrant ────────────────────────────────────────────────────────────────────

if [ -z "${QDRANT_URL:-}" ]; then
  skip "QDRANT_URL not set — skipping Qdrant backup"
else
  require_cmd curl
  require_cmd jq

  QDRANT_DIR="${BACKUP_DIR}/qdrant"
  mkdir -p "${QDRANT_DIR}"

  log "Listing Qdrant collections at ${QDRANT_URL}"
  COLLECTIONS_JSON="$(curl -fsSL "${QDRANT_URL}/collections")"
  COLLECTIONS="$(printf '%s' "${COLLECTIONS_JSON}" | jq -r '.result.collections[].name // empty')"

  if [ -z "${COLLECTIONS}" ]; then
    log "No Qdrant collections found — nothing to back up"
  else
    while IFS= read -r COLL; do
      [ -z "${COLL}" ] && continue
      log "Creating Qdrant snapshot for collection: ${COLL}"
      SNAP_RESP="$(curl -fsSL -XPOST "${QDRANT_URL}/collections/${COLL}/snapshots")"
      SNAP_NAME="$(printf '%s' "${SNAP_RESP}" | jq -r '.result.name // empty')"
      if [ -z "${SNAP_NAME}" ]; then
        die "Failed to create snapshot for collection '${COLL}': ${SNAP_RESP}"
      fi
      SNAP_FILE="${QDRANT_DIR}/${COLL}.snapshot"
      log "Downloading snapshot '${SNAP_NAME}' → ${SNAP_FILE}"
      curl -fsSL -o "${SNAP_FILE}" \
        "${QDRANT_URL}/collections/${COLL}/snapshots/${SNAP_NAME}"
      log "Qdrant collection '${COLL}' saved ($(du -sh "${SNAP_FILE}" | cut -f1))"
    done <<< "${COLLECTIONS}"
  fi
fi

# ── summary ───────────────────────────────────────────────────────────────────

log "Backup complete: ${BACKUP_DIR}"
printf '%s\n' "${BACKUP_DIR}"
