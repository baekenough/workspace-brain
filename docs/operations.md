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

## Production adapter readiness

The committed local runtime is not a complete SaaS production data stack. Before production, choose and operate explicit adapters for:

- durable catalog, binding, and job stores
- durable source/chunk storage
- tenant-scoped vector search
- retryable ingest workers and DLQ
- metrics, audit logs, traces, and backup alerts
- model provider credentials and deterministic test seams

Preserve the current contract when replacing internals: tenant scope, metadata-only discovery, grounded sources/spans, safe access errors, and adapter/core separation.
