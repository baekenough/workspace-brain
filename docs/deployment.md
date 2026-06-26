# Deployment Runbook

This runbook describes how to start, verify, operate, and roll back the current `workspace-brain` runtime.

`workspace-brain` is a surface-neutral RAG control plane. The committed runtime can serve the JSON HTTP adapter, the Slack adapter, or both from one process. It uses a local memory core with optional JSON snapshot persistence. Treat durable multi-node storage, external queues, and managed vector stores as future production adapters, not as committed runtime features.

## Production readiness boundary

Use the current runtime for local development, container packaging, integration testing, and small internal trials with clear backup expectations.

Do not describe this repository as SaaS-scale production-ready yet. The serving contract is production-shaped, but these production pieces are not committed:

- managed tenant catalog and binding storage
- managed source/chunk storage
- managed vector index storage
- retryable queue and DLQ for long ingest jobs
- full orchestrator manifests such as Compose, Kubernetes, or Terraform
- external durable store adapters such as PostgreSQL, Qdrant, and RabbitMQ
- wired model-provider adapters in the default server path

The committed durable path is `DATA_PATH/memory.json`: a single-process, local JSON snapshot. It survives process restarts on the same writable volume. It is not a replicated database, multi-node coordination layer, or managed backup system.

## Deployment modes

| Mode | Status | Dependencies | Use for |
|---|---|---|---|
| Demo CLI | Implemented | None | Fast gateway/core smoke test. |
| Local JSON HTTP server | Implemented | `API_TOKEN` | Development and non-Slack integration tests. |
| Local Slack server | Implemented | `SLACK_SIGNING_SECRET`; public HTTPS ingress for real Slack use | Slack adapter trials. |
| Local persistent server | Implemented | `DATA_PATH` writable filesystem | Restart-tolerant local trials. |
| Container image | Implemented | Docker runtime and adapter secrets | Repeatable packaging and deployment handoff. |
| Production durable deployment | Roadmap | Durable stores, queue, observability, backup, orchestrator manifests | Tenant durability, scale, audit, and recovery. |

## Environment contract

Server mode requires at least one frontend adapter:

- Set `API_TOKEN` to enable `POST /api/commands`.
- Set `SLACK_SIGNING_SECRET` to enable Slack routes.
- Set both to expose both adapters in one process.

| Variable | Required | Default | Runbook use |
|---|---:|---|---|
| `ADDR` | No | `:8080` | HTTP listen address. |
| `API_TOKEN` | Required unless Slack is configured | none | Bearer token for the JSON HTTP adapter. |
| `SLACK_SIGNING_SECRET` | Required unless JSON API is configured | none | Slack HMAC signing secret. |
| `ADMIN_USERS` | No | empty | Comma-separated Slack user IDs that receive the `admin` role. |
| `PUBLIC_BASE_URL` | No | none | Absolute public base URL. When set with Slack, enables `GET /slack/manifest.json`. Use HTTPS for Slack registration. |
| `COMMAND_NAME` | No | `brain` | Slack slash command name without `/`. Keep stable after Slack registration. |
| `SLACK_APP_NAME` | No | `workspace-brain` | Display name in the generated Slack manifest. |
| `DATA_PATH` | No | none | Directory for local JSON memory snapshot at `$DATA_PATH/memory.json`. The server creates it with `0700` permissions. |
| `READINESS_REQUIRED` | No | `false` | When true, `/readyz` checks configured local dependencies such as `DATA_PATH`. |
| `HTTP_READ_HEADER_TIMEOUT` | No | `5s` | Go duration for request-header read timeout. |
| `HTTP_READ_TIMEOUT` | No | `15s` | Go duration for full request read timeout. |
| `HTTP_WRITE_TIMEOUT` | No | `30s` | Go duration for response write timeout. |
| `HTTP_IDLE_TIMEOUT` | No | `60s` | Go duration for keep-alive idle timeout. |
| `HTTP_SHUTDOWN_TIMEOUT` | No | `10s` | Go duration for graceful shutdown. |
| `OPENAI_BASE_URL` | No | `https://api.openai.com/v1` in the optional client | Optional provider seam only. Not used by the default server path. |
| `OPENAI_API_KEY` | No | none | Required only when constructing the optional OpenAI-compatible client. |
| `OPENAI_EMBEDDING_MODEL` | No | none | Required only for optional embedding calls. |
| `OPENAI_RESPONSE_MODEL` | No | none | Required only for optional response calls. |
| `OPENAI_ORG_ID` | No | none | Optional provider header. |
| `OPENAI_PROJECT_ID` | No | none | Optional provider header. |

Timeout values must be positive Go durations such as `5s`, `30s`, or `1m`.

## Local runbook

### Start a local JSON HTTP server

```bash
export API_TOKEN='dev-token'
export ADDR=':8080'
go run ./cmd/workspace-brain
```

The process refuses to start unless `API_TOKEN` or `SLACK_SIGNING_SECRET` is set.

### Check liveness and readiness

```bash
curl -fsS http://localhost:8080/healthz
curl -fsS http://localhost:8080/readyz
```

Expected responses:

```json
{"status":"ok"}
```

```json
{"status":"ready"}
```

### Run a JSON API smoke test

Create a project as an admin:

```bash
curl -fsS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"admin","roles":["admin"]},"text":"create Demo"}'
```

Ingest inline text:

```bash
curl -fsS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ingest text://workspace-brain stores tenant knowledge"}'
```

Check job status using the random hex job ID returned by the ingest response:

```bash
curl -fsS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"status job-616eec1b005fb0258433d73b843f8380"}'
```

Replace `job-616eec1b005fb0258433d73b843f8380` with the job ID from your ingest response. IDs are random hex generated by the gateway.

Ask a grounded question:

```bash
curl -fsS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ask what does workspace-brain store?"}'
```

The JSON adapter accepts only `POST`, requires `Authorization: Bearer <API_TOKEN>`, rejects unknown JSON fields, and caps request bodies at 1 MiB.

## Container runbook

### Build the image

```bash
make docker-build
```

Equivalent direct command:

```bash
docker build -t workspace-brain:local .
```

The Dockerfile builds a static Go binary in a Go build stage and copies it into a distroless non-root runtime image. The runtime image does not include shell tools.

### Run the JSON HTTP adapter in a container

```bash
docker run --rm -p 127.0.0.1:18080:8080 \
  -e API_TOKEN=dev-token \
  workspace-brain:local
```

Verify the container:

```bash
curl -fsS http://localhost:18080/healthz
curl -fsS http://localhost:18080/readyz
```

### Run with local JSON persistence

```bash
DATA_DIR="$(mktemp -d)"
chmod 0777 "$DATA_DIR"

docker run --rm -p 127.0.0.1:18080:8080 \
  -e API_TOKEN=dev-token \
  -e DATA_PATH=/data \
  -e READINESS_REQUIRED=true \
  -v "$DATA_DIR:/data" \
  workspace-brain:local
```

Operational notes:

- Mount a writable volume when `DATA_PATH` is set. The image runs as the distroless `nonroot` user, so provision volume ownership or permissions before startup.
- Keep secrets in the runtime environment or platform secret manager. Do not bake them into image layers.
- Add TLS termination, restart policy, log collection, and volume backup in the deployment platform. They are not provided by the Dockerfile.
- Use `GET /healthz` for liveness and `GET /readyz` for readiness.

## Slack adapter runbook

### Start Slack routes

```bash
export SLACK_SIGNING_SECRET='replace-me'
export ADMIN_USERS='U123,U456'
export ADDR=':8080'
go run ./cmd/workspace-brain
```

Mounted routes:

```text
POST /slack/commands
POST /slack/interactions
```

The Slack adapter verifies the Slack HMAC signature, rejects stale timestamps, validates `COMMAND_NAME`, and renders private responses. Do not bypass Slack signature verification in production.

### Generate a Slack manifest

Set a public HTTPS base URL before registering the app:

```bash
export SLACK_SIGNING_SECRET='replace-me'
export PUBLIC_BASE_URL='https://workspace-brain.example.com'
export SLACK_APP_NAME='Workspace Brain'
export COMMAND_NAME='brain'
go run ./cmd/workspace-brain
```

Fetch the generated manifest:

```bash
curl -fsS http://localhost:8080/slack/manifest.json
```

Register the manifest with Slack, then keep `COMMAND_NAME` stable. The runtime validates the command value, but it does not persist a command-name lock yet.

### Slack operational checks

- Configure Slack slash command Request URL as `${PUBLIC_BASE_URL}/slack/commands`.
- Configure interactivity Request URL as `${PUBLIC_BASE_URL}/slack/interactions`.
- Store `SLACK_SIGNING_SECRET` and `ADMIN_USERS` as deployment secrets or protected config.
- Grant admin capability through `ADMIN_USERS`; do not rely on Slack scopes as the gateway authorization boundary.
- Verify a real signed `/brain create Demo` request from Slack after deploy.
- Verify a non-admin user cannot create a project and receives a safe access error.

## Persistence runbook

### Enable local persistence

```bash
export API_TOKEN='dev-token'
export DATA_PATH="$(mktemp -d)"
export READINESS_REQUIRED=true
go run ./cmd/workspace-brain
```

The local memory core writes an atomic JSON snapshot to:

```text
$DATA_PATH/memory.json
```

The snapshot includes bindings, projects, jobs, sources, and local documents. It uses local file permissions, not encryption or managed access control.

### Back up and restore local state

Back up the snapshot only while writes are quiesced or by using a filesystem snapshot:

```bash
cp "$DATA_PATH/memory.json" ./memory.backup.json
```

Restore by stopping the process, replacing `memory.json`, and starting the same binary or a compatible version:

```bash
cp ./memory.backup.json "$DATA_PATH/memory.json"
```

Before production use, replace this local JSON file with durable adapters and a tested backup/restore procedure.

## Source ingest runbook

The default server gateway resolves source references before handing metadata to the memory core.

Supported source shapes:

- `file://` URLs with local or `localhost` hosts only
- `http://` and `https://` URLs
- inline metadata keys: `content`, `inline_content`, `inline`, `raw`, or `text`
- `text://...` and `raw://...` URI forms
- fallback plain text from `SourceRef.URI` or `SourceRef.Name`

Current safety bounds:

- Default source size limit: 1 MiB.
- Default source load timeout: 5 seconds when the caller has no earlier deadline.
- Non-2xx HTTP responses fail ingest.
- Empty content fails ingest.

Operational cautions:

- Accept untrusted files only behind allowlists, MIME policy, malware scanning, and audit logging.
- Move large or slow ingest paths to a retryable worker with DLQ before production scale.
- Keep failed ingest jobs visible to operators and replay them explicitly after recovery.
- Treat source content as tenant data in logs, traces, and backups.

## Readiness runbook

`/healthz` checks only that the process can respond. It returns `{"status":"ok"}` and does not check dependencies.

`/readyz` returns `{"status":"ready"}` when the process can serve traffic:

- If `READINESS_REQUIRED=false`, readiness has no dependency checker.
- If `READINESS_REQUIRED=true` and `DATA_PATH` is set, readiness verifies that `DATA_PATH` exists and is a directory.
- If the dependency check fails, `/readyz` returns HTTP 503 with `{"status":"not_ready","error":"dependency readiness check failed"}`.

Use readiness failure to stop new traffic. Use liveness failure to restart the process.

## Release gates

Run these gates before publishing an image or promoting a deployment:

```bash
make test
make race
make coverage
make vet
make docker-build
```

Equivalent raw commands:

```bash
go test ./...
go test -race ./...
go test ./... -coverprofile=coverage.out
./scripts/check-coverage.sh coverage.out
go vet ./...
docker build -t workspace-brain:local .
```

Release checklist:

- [ ] Server starts with the selected adapter configuration.
- [ ] `/healthz` and `/readyz` pass.
- [ ] JSON API smoke passes when `API_TOKEN` is enabled.
- [ ] Slack manifest fetch passes when `PUBLIC_BASE_URL` is enabled.
- [ ] A real signed Slack slash command passes when Slack is enabled.
- [ ] `DATA_PATH` volume backup and restore are tested if local persistence is used.
- [ ] Secrets come from the deployment platform, not committed files or image layers.
- [ ] Rollback image or binary remains available until post-deploy smoke passes.
- [ ] Vulnerability scan results are reviewed in CI or the release environment.

## End-to-end smoke tests

### Source-run smoke

```bash
go test ./...
go run ./cmd/workspace-brain demo
```

### Server smoke

Start a fresh local server, then run the JSON API commands from the local runbook:

```bash
DATA_DIR="$(mktemp -d)"
API_TOKEN=dev-token DATA_PATH="$DATA_DIR" READINESS_REQUIRED=true ADDR=:8080 go run ./cmd/workspace-brain &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null || true; rm -rf "$DATA_DIR"' EXIT

ready=0
for _ in $(seq 1 20); do
  if curl -fsS http://localhost:8080/readyz; then
    ready=1
    break
  fi
  sleep 0.25
done
test "$ready" = 1
```

Then run create, ingest, status, and ask through `POST /api/commands`.

### Container smoke

```bash
docker build -t workspace-brain:local .
DATA_DIR="$(mktemp -d)"
chmod 0777 "$DATA_DIR"
docker run -d --name workspace-brain-smoke -p 127.0.0.1:18080:8080 \
  -e API_TOKEN=dev-token \
  -e DATA_PATH=/data \
  -e READINESS_REQUIRED=true \
  -v "$DATA_DIR:/data" \
  workspace-brain:local

ready=0
for _ in $(seq 1 20); do
  if curl -fsS http://localhost:18080/readyz; then
    ready=1
    break
  fi
  sleep 0.25
done
test "$ready" = 1

curl -fsS http://localhost:18080/healthz
docker rm -f workspace-brain-smoke
rm -rf "$DATA_DIR"
```

If any smoke step fails, keep the previous release active and inspect logs before retrying.

## Rollback runbook

1. Stop new ingress to the failing version.
2. Keep the previous binary or image available before deploy.
3. Roll back stateless runtime first.
4. If `DATA_PATH` changed, stop the process before replacing `memory.json`.
5. Restore only a snapshot that matches the expected snapshot version and release contract.
6. Start the previous runtime with the previous environment.
7. Verify `/readyz`.
8. Run one tenant-scoped create/ingest/status/ask smoke or a safe existing-tenant query.
9. Record failed ingest jobs that need replay.
10. Re-enable ingress only after smoke passes.

For future durable adapters, require backward-compatible migrations or a tested database/vector-store rollback plan before promotion.
