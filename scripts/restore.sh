#!/usr/bin/env bash
# restore.sh — restore workspace-brain durable stores from a backup directory.
#
# Reads artifacts from BACKUP_DIR and restores each configured store.  Prompts
# for confirmation before any destructive operation unless FORCE=1 is set.
# Stores whose backup artifacts are absent within BACKUP_DIR are skipped.
#
# Environment variables
# ---------------------
# BACKUP_DIR    (Required) Path to the timestamped backup directory produced by
#               backup.sh, e.g. ./backups/20240101T120000Z.
# DATA_PATH     Destination data directory for memory.json.  Must be set when
#               a memory.json artifact exists in BACKUP_DIR.
# PGDSN         PostgreSQL connection string for pg_restore.
# QDRANT_URL    Qdrant base URL, e.g. http://localhost:6333.
# FORCE         Set to 1 to skip confirmation prompts (e.g. for CI).
#
# Usage
# -----
#   BACKUP_DIR=./backups/20240101T120000Z \
#   PGDSN=postgres://wb:secret@localhost:5432/wbdb \
#   DATA_PATH=/var/lib/workspace-brain \
#   QDRANT_URL=http://localhost:6333 \
#   ./scripts/restore.sh
#
set -euo pipefail

# ── helpers ───────────────────────────────────────────────────────────────────

log()  { printf '[restore] %s\n' "$*" >&2; }
skip() { printf '[restore] SKIP %s\n' "$*" >&2; }
die()  { printf '[restore] ERROR %s\n' "$*" >&2; exit 1; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

confirm() {
  local msg="$1"
  if [ "${FORCE:-0}" = "1" ]; then
    log "FORCE=1 — skipping confirmation: ${msg}"
    return 0
  fi
  printf '[restore] WARNING: %s\nProceed? [y/N] ' "${msg}" >&2
  read -r REPLY </dev/tty
  case "${REPLY}" in
    y|Y|yes|YES) return 0 ;;
    *) log "Aborted by user."; exit 1 ;;
  esac
}

# ── validate required input ────────────────────────────────────────────────────

BACKUP_DIR="${BACKUP_DIR:-}"
if [ -z "${BACKUP_DIR}" ]; then
  die "BACKUP_DIR is required.  Set it to the timestamped directory from backup.sh."
fi
if [ ! -d "${BACKUP_DIR}" ]; then
  die "BACKUP_DIR does not exist or is not a directory: ${BACKUP_DIR}"
fi

log "Restoring from: ${BACKUP_DIR}"

# ── JSON memory snapshot ───────────────────────────────────────────────────────

SNAP_SRC="${BACKUP_DIR}/memory.json"
if [ ! -f "${SNAP_SRC}" ]; then
  skip "No memory.json in backup directory — skipping snapshot restore"
else
  if [ -z "${DATA_PATH:-}" ]; then
    die "memory.json found in backup but DATA_PATH is not set"
  fi
  SNAP_DST="${DATA_PATH}/memory.json"
  confirm "This will overwrite ${SNAP_DST} with the backup copy"
  log "Restoring memory snapshot → ${SNAP_DST}"
  mkdir -p "${DATA_PATH}"
  # Match the application's expected 0600 permission on the snapshot file.
  install -m 0600 "${SNAP_SRC}" "${SNAP_DST}"
  log "memory.json restored"
fi

# ── PostgreSQL ─────────────────────────────────────────────────────────────────

PG_DUMP_FILE="${BACKUP_DIR}/postgres.dump"
if [ ! -f "${PG_DUMP_FILE}" ]; then
  skip "No postgres.dump in backup directory — skipping PostgreSQL restore"
else
  if [ -z "${PGDSN:-}" ]; then
    die "postgres.dump found in backup but PGDSN is not set"
  fi
  require_cmd pg_restore
  confirm "This will restore the PostgreSQL dump into ${PGDSN} (existing data may be overwritten)"
  log "Running pg_restore from ${PG_DUMP_FILE} → ${PGDSN}"
  # --no-owner and --no-privileges let the restore succeed regardless of role
  # differences between the source and target databases.
  # --clean drops and recreates objects before restoring them.
  # --if-exists prevents errors when objects are absent before the first restore.
  pg_restore \
    --no-password \
    --clean \
    --if-exists \
    --no-owner \
    --no-privileges \
    --dbname="${PGDSN}" \
    "${PG_DUMP_FILE}"
  log "pg_restore complete"
fi

# ── Qdrant ────────────────────────────────────────────────────────────────────

QDRANT_DIR="${BACKUP_DIR}/qdrant"
if [ ! -d "${QDRANT_DIR}" ]; then
  skip "No qdrant/ directory in backup — skipping Qdrant restore"
else
  if [ -z "${QDRANT_URL:-}" ]; then
    die "qdrant/ snapshots found in backup but QDRANT_URL is not set"
  fi
  require_cmd curl
  require_cmd jq

  SNAP_FILES="$(find "${QDRANT_DIR}" -name '*.snapshot' 2>/dev/null || true)"
  if [ -z "${SNAP_FILES}" ]; then
    skip "No .snapshot files in ${QDRANT_DIR} — skipping Qdrant restore"
  else
    confirm "This will upload Qdrant snapshots from ${QDRANT_DIR} and recover each collection (existing collection data will be replaced)"
    while IFS= read -r SNAP_FILE; do
      [ -z "${SNAP_FILE}" ] && continue
      COLL="$(basename "${SNAP_FILE}" .snapshot)"
      log "Uploading snapshot for collection '${COLL}' from ${SNAP_FILE}"
      RESP="$(curl -fsSL -XPOST \
        -H 'Content-Type: multipart/form-data' \
        -F "snapshot=@${SNAP_FILE}" \
        "${QDRANT_URL}/collections/${COLL}/snapshots/upload?priority=snapshot")"
      STATUS="$(printf '%s' "${RESP}" | jq -r '.status // empty')"
      if [ "${STATUS}" != "ok" ]; then
        die "Qdrant snapshot upload for '${COLL}' failed: ${RESP}"
      fi
      log "Qdrant collection '${COLL}' restored"
    done <<< "${SNAP_FILES}"
  fi
fi

# ── summary ───────────────────────────────────────────────────────────────────

log "Restore complete from: ${BACKUP_DIR}"
log "Verify the process with GET /readyz and a tenant-scoped smoke query."
