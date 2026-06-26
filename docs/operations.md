# Operations

workspace-brain currently operates as a single Go process with a local deterministic data core. Run it as a surface-neutral RAG service: adapters authenticate requests, the gateway authorizes tenant actions, and the data core stores tenant knowledge.

## Runtime modes

| Mode | Required config | Data behavior | Use case |
|---|---|---|---|
| Demo | none | In-memory only | Local verification with `go run ./cmd/workspace-brain demo`. |
| JSON HTTP server | `API_TOKEN` | In-memory or `DATA_PATH` snapshot | Local and internal API trials. |
| Slack adapter server | `SLACK_SIGNING_SECRET` | In-memory or `DATA_PATH` snapshot | Slack as one frontend adapter. |
| Dual adapter server | `API_TOKEN` and `SLACK_SIGNING_SECRET` | Shared gateway and core | Compare adapters against the same contract. |

At least one frontend adapter must be configured in server mode.

## Health and readiness

The server always mounts:

```text
GET /healthz
GET /readyz
```

Behavior:

- `/healthz` returns `200` with `{"status":"ok"}` when the process can serve the handler.
- `/readyz` returns `200` with `{"status":"ready"}` by default.
- Set `READINESS_REQUIRED=true` to enable configured dependency checks.
- When `READINESS_REQUIRED=true` and `DATA_PATH` is set, readiness verifies that `DATA_PATH` exists and is a directory.
- Failed readiness returns `503` with `{"status":"not_ready","error":"dependency readiness check failed"}`.

Use `/healthz` for liveness. Use `/readyz` for rollout and traffic routing.

## Token and adapter authentication

JSON HTTP:

- Set `API_TOKEN`.
- Send `Authorization: Bearer <token>` to `POST /api/commands`.
- The JSON adapter rejects missing or invalid bearer tokens.
- The adapter limits request bodies to 1 MiB and rejects unknown JSON fields.

Slack adapter:

- Set `SLACK_SIGNING_SECRET`.
- Configure `ADMIN_USERS` with comma-separated Slack user IDs that receive the admin role.
- Keep Slack request signatures, timestamps, slash command names, and interaction payloads inside `internal/control/slack`.
- Treat Slack as an adapter example. Do not store Slack channel or user concepts in the data core.

Gateway authorization:

- Admins can create projects and toggle project state.
- Members can read and write existing tenants through authorized commands.
- Missing bindings and unauthorized access are rendered as safe access errors.
- Job lookup stays scoped by `(tenant_id, job_id)`.

## Data path

`DATA_PATH` enables local JSON snapshot persistence.

```bash
export DATA_PATH=/var/lib/workspace-brain
export READINESS_REQUIRED=true
```

Operational facts:

- Startup creates `DATA_PATH` with `0700` permissions.
- The snapshot path is `$DATA_PATH/memory.json`.
- Snapshot writes use a temp file, `fsync`, atomic rename, and directory sync.
- Snapshot files use `0600` permissions.
- The snapshot stores bindings, projects, jobs, sources, and documents.
- Chunks and local vectors are derived from documents on load.

Keep `DATA_PATH` on durable local storage for restart-tolerant trials. Do not share one `DATA_PATH` between multiple live processes.

## Backup and restore

Current local runtime:

1. Stop the process or pause writes.
2. Copy `$DATA_PATH/memory.json` and the environment/deployment config.
3. Store the backup off-host.
4. Restore by placing `memory.json` back under the configured `DATA_PATH`.
5. Start the process.
6. Verify `/readyz`, create or resolve a tenant, and run one grounded `ask`.

If `memory.json` is missing, the persistent core starts empty. If the file is unreadable, invalid JSON, or has an unsupported snapshot version, the core reports internal persistence errors and should not receive traffic.

Future external stores:

- Back up catalog and binding state.
- Back up source documents and chunk storage.
- Back up job state or define a replay policy.
- Back up vector indexes or preserve enough source content to rebuild them.
- Test restore before treating the deployment as production-ready.

## Incident response and rollback

Use this sequence for incidents:

1. Check `/healthz` and `/readyz`.
2. Inspect recent logs for request IDs, tenant scope, job IDs, and typed error kinds.
3. Stop routing new traffic if readiness fails.
4. Preserve `$DATA_PATH/memory.json` before manual repair.
5. Roll back to the previous binary or image.
6. Restore the last known-good snapshot if data corruption caused the incident.
7. Run the smoke path: health, readiness, create or resolve tenant, ingest, status, and ask.
8. Re-enable traffic only after the smoke path passes.

Keep the previous release image available until the new release passes smoke tests. For future database/vector migrations, require a tested backward-compatible migration or a documented rollback path.

## Adapter operations tips

JSON HTTP adapter:

- Prefer JSON HTTP for smoke tests because it needs only `API_TOKEN`.
- Rotate `API_TOKEN` through deployment configuration, not code.
- Do not log bearer tokens.

Slack adapter:

- Use Slack to validate adapter behavior, not core behavior.
- Keep `COMMAND_NAME` stable after registering the Slack app.
- Set `PUBLIC_BASE_URL` to an HTTPS URL before using `/slack/manifest.json` for app setup.
- Use `ADMIN_USERS` for local admin mapping. Replace or harden this policy before broad production use.

Future adapters:

- Authenticate at the adapter edge.
- Convert surface identity to `brainapi.Principal`.
- Convert surface location to `brainapi.BindingKey`.
- Call the shared dispatcher and gateway.
- Never call the data core directly.

## Source loader operations

The default source loader protects ingest:

- File sources must be local `file://` paths.
- HTTP(S) sources must return 2xx.
- Inline, `text://`, and `raw://` sources work without network access.
- Default max source size is 1 MiB.
- Default source timeout is 5 seconds.
- Empty content fails ingest.

For larger or slower sources, add an explicit future ingest worker path with retry, DLQ, and operator-visible job state. Do not silently raise limits without tests and deployment capacity checks.

## Backup, restore, and migrations

### How migrations are applied

workspace-brain embeds all SQL migration files under `internal/core/postgres/migrations/` using Go's `//go:embed` directive.  When the PostgreSQL adapter opens a connection it calls `migrate()`, which:

1. Reads all `*.sql` files from the embedded filesystem.
2. Sorts them lexicographically (e.g. `001_initial.sql`, `002_…sql`).
3. Executes each file inside its own transaction — a partial failure leaves the schema unchanged and returns an error.
4. Runs on every startup, so re-running against a schema that already has the objects is safe (each file uses `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`).

No migration-tracking table is maintained at this time; idempotent DDL is the guard.  When a new SQL file is added, increment the numeric prefix to guarantee ordering.

**Applying migrations in production**

Migrations run automatically on adapter startup.  For a planned schema change:

1. Add the new `NNN_description.sql` file under `internal/core/postgres/migrations/`.
2. Deploy the new binary.  The adapter applies the migration on first connection.
3. If the migration fails the adapter exits and the previous schema is intact (each migration is a single transaction).
4. For destructive migrations (DROP, rename) coordinate a maintenance window or use an expand/contract pattern with two separate deployments.

### Durable stores overview

| Store | Configured by | Backup artifact | Restore tool |
|---|---|---|---|
| JSON memory snapshot | `DATA_PATH` | `memory.json` | `cp` / `install` |
| PostgreSQL | `PGDSN` | `postgres.dump` (custom format) | `pg_restore` |
| Qdrant vector store | `QDRANT_URL` | `qdrant/<collection>.snapshot` | Qdrant REST upload |

### RPO / quiesce guidance

- **JSON snapshot** — the memory core writes atomically (temp file → fsync → rename).  A copy taken at any moment captures a consistent snapshot.  For highest fidelity, quiesce writes by stopping the process or draining traffic before backing up.
- **PostgreSQL** — `pg_dump` issues a consistent snapshot via MVCC; the database does not need to be stopped.  For point-in-time recovery, enable WAL archiving separately.
- **Qdrant** — the snapshot API produces a collection-level consistent snapshot.  For large collections, prefer backing up during low-traffic periods to reduce snapshot creation time.

### Backing up with scripts/backup.sh

```bash
# Minimal: JSON snapshot only
DATA_PATH=/var/lib/workspace-brain \
  ./scripts/backup.sh

# All stores
DATA_PATH=/var/lib/workspace-brain \
PGDSN=postgres://wb:secret@localhost:5432/wbdb \
QDRANT_URL=http://localhost:6333 \
BACKUP_ROOT=/mnt/backups \
  ./scripts/backup.sh
```

The script writes a timestamped subdirectory under `BACKUP_ROOT` (default: `./backups`) and prints the path on stdout so callers can capture it:

```bash
BACKUP_DIR="$(./scripts/backup.sh)"
```

Artifacts produced:

| File | Store |
|---|---|
| `memory.json` | JSON memory snapshot |
| `postgres.dump` | PostgreSQL custom-format dump |
| `qdrant/<name>.snapshot` | One file per Qdrant collection |

The script skips any store whose environment variable is not set and fails loudly (non-zero exit) on any error in a configured store.

### Restoring with scripts/restore.sh

```bash
BACKUP_DIR=./backups/20240101T120000Z \
DATA_PATH=/var/lib/workspace-brain \
PGDSN=postgres://wb:secret@localhost:5432/wbdb \
QDRANT_URL=http://localhost:6333 \
  ./scripts/restore.sh
```

The script prompts for confirmation before each destructive write.  Set `FORCE=1` to skip prompts in automated pipelines.

Recommended restore sequence:

1. Stop the workspace-brain process (or drain traffic).
2. Run `restore.sh` with the target backup directory.
3. Start the process.
4. Verify `GET /healthz` and `GET /readyz`.
5. Run a tenant-scoped smoke query (create project, ingest, ask).
6. Re-enable traffic.

The script will error and abort if a backup artifact exists but the corresponding environment variable (e.g. `PGDSN`) is missing, preventing a partial restore.

### Testing the restore path with scripts/restore-smoke-test.sh

```bash
BACKUP_DIR=./backups/20240101T120000Z \
  ./scripts/restore-smoke-test.sh
```

The smoke test:

1. Checks that `docker` and `pg_isready`/`psql`/`pg_restore` are available; skips gracefully if not.
2. Starts an ephemeral `postgres:16-alpine` container on a random host port.
3. Restores `postgres.dump` from the backup directory.
4. Asserts that all schema tables (`tenants`, `bindings`, `sources`, `chunks`, `jobs`) are present.
5. Reports row counts for each table.
6. Tears down the container on exit (success or failure).

The smoke test is not wired into the unit gate.  Run it on demand after taking a backup or before promoting a restore to production.

Override the Postgres image or port if the default conflicts with local services:

```bash
PG_IMAGE=postgres:15-alpine \
SMOKE_PORT=25432 \
BACKUP_DIR=./backups/20240101T120000Z \
  ./scripts/restore-smoke-test.sh
```

### Syntax verification

Run a shell syntax check on all three scripts without executing them:

```bash
bash -n scripts/backup.sh scripts/restore.sh scripts/restore-smoke-test.sh
```

If `shellcheck` is available, it provides additional static analysis:

```bash
shellcheck scripts/backup.sh scripts/restore.sh scripts/restore-smoke-test.sh
```

## Production adapter readiness

The committed local runtime is not a complete SaaS production data stack. Before production, choose and operate explicit adapters for:

- durable catalog, binding, and job stores
- durable source/chunk storage
- tenant-scoped vector search
- retryable ingest workers and DLQ
- metrics, audit logs, traces, and backup alerts
- model provider credentials and deterministic test seams

Preserve the current contract when replacing internals: tenant scope, metadata-only discovery, grounded sources/spans, safe access errors, and adapter/core separation.
