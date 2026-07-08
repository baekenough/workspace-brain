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
- See "생성 권한 정책" below for the decided `create_project` (`/brain create`) authorization policy, allowlist configuration, and a known code/runtime wiring gap.

## 생성 권한 정책 (create_project authorization)

**결정된 정책**: `create_project`(`/brain create`)는 다음 중 하나에 해당하는 principal만 실행할 수 있다.

1. `admin` role을 가진 principal (예: `ADMIN_USERS`에 등록된 Slack user ID, API 토큰 어댑터에서 admin으로 매핑된 principal 등 어댑터별 admin 매핑 규칙을 따른다).
2. **create allowlist**에 등록된 principal — `admin` role 없이 project 생성만 허용하는 opt-in 예외 목록.

이 정책은 `internal/control/gateway/authorizer.go`의 `PolicyAuthorizer.Authorize`에 구현되어 있다.

- `admin` role → 모든 action 허용.
- `ActionCreateProject` → `admin` 또는 allowlist에 등록된 principal만 허용, 그 외는 `KindUnauthorized`.
- `ActionAdmin` → `admin` role만 허용.
- 그 외 action → `member` role 필요.

**Allowlist 운영 방법**:

- Env var: `CREATE_ALLOW_USERS` (comma-separated). `ADMIN_USERS`와 동일한 파싱 규칙(`internal/platform/config/config.go`의 `splitList` — trim, 빈 값 제거, 중복 제거)을 따른다.
- `Config.CreateAllowlistSet()`이 리스트를 lookup map(`map[string]bool`)으로 변환하여 `gateway.NewPolicyAuthorizer(...)`에 전달하도록 설계되어 있다.
- 등록 형식: 각 항목은 principal의 raw `ID`(예: `U2`) 또는 `Source:ID` 형식의 key(예: `slack:U2`, `PolicyAuthorizer.isAllowlisted`가 `Principal.Key()`와 `Principal.ID` 양쪽을 매칭) 중 하나로 지정할 수 있다.
- `CREATE_ALLOW_USERS`가 비어 있으면(기본값) allowlist 검사는 항상 실패하며, 정책은 사실상 admin-only와 동일하게 동작한다 — allowlist는 순수 opt-in 확장이다.
- `create_project`와 `admin` action은 `isSensitiveAction`으로 분류되어 `CachingAuthorizer`의 TTL 캐시를 우회하고 매 요청마다 delegate authorizer를 live 재호출한다. 즉 admin/allowlist 구성을 바꾸면 즉시 반영되며, member read/write action처럼 캐시로 인한 지연 반영이 없다.

**정책 요약표**:

| Principal 상태 | `create_project` 허용 여부 |
|---|---|
| `admin` role | 허용 |
| `admin` role 없음 + `CREATE_ALLOW_USERS`(또는 key) 등록됨 | 허용 (PolicyAuthorizer 배선 시) |
| `admin` role 없음 + allowlist 미등록, `member` role만 있음 | 거부 |
| role 없음(빈 principal 포함) | 거부 |

**⚠️ 코드-문서 정합성 확인 결과 (2026-07-08 기준, 반드시 확인)**:

`cmd/workspace-brain/main.go`의 `runServer`와 데모 경로는 게이트웨이를 `gateway.RoleAuthorizer{}`로 생성한다(`newAppGateway(core, gateway.RoleAuthorizer{}, jobStore)`). `RoleAuthorizer`는 `PolicyAuthorizer`와 달리 allowlist 개념이 전혀 없고, `create_project`/`admin`은 무조건 `admin` role만 허용한다.

즉 `PolicyAuthorizer`, `Config.CreateAllowlistSet()`, `CREATE_ALLOW_USERS`는 **단위 테스트(`internal/control/gateway/authorizer_test.go`, `internal/platform/config/config_test.go`)로 구현·검증은 완료되었으나, 실행 바이너리(`cmd/workspace-brain/main.go`)에는 아직 배선되지 않았다.** 현재 운영 중인 서버에 `CREATE_ALLOW_USERS`를 설정해도 아무 효과가 없다 — `RoleAuthorizer`가 이 값을 참조하지 않기 때문이다.

이 섹션은 **결정된 정책**(admin 또는 allowlist)을 기록한다. 위 배선 갭(`main.go`에서 `gateway.RoleAuthorizer{}`를 `gateway.NewPolicyAuthorizer(cfg.CreateAllowlistSet())`로 교체)은 별도 구현 작업으로 추적해야 한다 — 정책 결정·문서화(G7) 범위에는 코드 변경이 포함되지 않는다.

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
