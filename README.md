# workspace-brain

workspace-brain is a **surface-neutral RAG control plane** for collecting, isolating, and querying project knowledge by tenant.

Slack is not the product boundary. Slack is one frontend adapter beside the JSON HTTP adapter and future Web, CLI, or Admin UI adapters. The shared center is the gateway/core contract that accepts tenant-scoped commands, source references, and questions.

## What it is

workspace-brain keeps frontend surfaces separate from tenant-scoped RAG operations:

```text
Frontend Adapter -> Frontend Dispatcher -> Control Gateway -> Data Core Contract
```

- **Frontend Adapter** verifies surface-specific authentication and converts surface input into a `Principal`, `BindingKey`, and text command.
- **Frontend Dispatcher** parses shared text commands and calls gateway operations.
- **Control Gateway** resolves bindings, authorizes actions, creates `tenant_id` and `job_id` values, loads bounded source content, and normalizes safe access errors.
- **Data Core** does not know about Slack, Web, CLI, or HTTP sessions. It receives only contract inputs such as `tenant_id`, `job_id`, `source_ref`, and `question`.

## Current capabilities

Implemented in this repository:

- Surface-neutral `brainapi.Core` contract.
- Gateway-managed project creation, ingest jobs, status reconciliation, safe access errors, and optional `admin on|off <tenant_id>` project state commands.
- Local RAG memory core for development, tests, and local deployments:
  - tenant catalog and binding registry
  - source metadata and content ingest
  - deterministic local embeddings
  - lexical + vector scoring
  - grounded answer/source/span response
  - metadata-only discovery
  - optional JSON snapshot persistence through `DATA_PATH`
- Frontend adapters:
  - JSON HTTP command API at `POST /api/commands`
  - Slack slash-command adapter at `POST /slack/commands`
  - Slack interactivity acknowledgement at `POST /slack/interactions`
  - Slack manifest-as-code at `GET /slack/manifest.json` when `PUBLIC_BASE_URL` is configured
- Source loader wired into the default gateway/server path for `file://`, `http://`, `https://`, `text:`/`text://`, `raw:`/`raw://`, plain/raw text, and inline metadata content.
- Health and readiness endpoints at `GET /healthz` and `GET /readyz`.
- Hardened HTTP server helper with read/write/idle/shutdown timeouts.
- Dockerfile for a multi-stage, distroless, non-root image build.
- Quality gates for release: unit tests, race test, vet, and a 100% coverage script.

Current boundaries:

- The default serve path uses the local memory core and source loader. It does not call an external model provider.
- `internal/core/ai` contains an optional OpenAI-compatible client seam. It is not wired into the default server path.
- PostgreSQL, Qdrant, RabbitMQ, background ingest workers, Compose files, and orchestrator manifests are roadmap adapters, not committed runtime dependencies.
- Server-mode ingest stores chunks immediately and returns a running job. No committed background worker marks jobs completed in server mode yet.

## Quick start

Prerequisites:

- Go `1.25.11` or newer compatible Go 1.25 toolchain.
- Docker only if you build the container image.

Run the local verification and demo path:

```bash
go test ./...
go test -race ./...
go run ./cmd/workspace-brain demo
```

Expected demo shape:

```text
tenant=tenant-3c4997260a941958762c934928ad17ba job=job-616eec1b005fb0258433d73b843f8380 answer=...
```

The demo uses the local in-memory core. It does not require Slack, OpenAI, PostgreSQL, Qdrant, RabbitMQ, Docker, or any external key.

## Run the server

The server starts only when at least one frontend adapter is configured:

- Set `API_TOKEN` to enable the JSON HTTP API.
- Set `SLACK_SIGNING_SECRET` to enable the Slack adapter.
- Set both to expose both adapters in one process.

Start the JSON HTTP adapter locally:

```bash
export API_TOKEN='dev-token'
export ADDR=':8080'
go run ./cmd/workspace-brain
```

Check liveness and readiness from another shell:

```bash
curl -sS http://localhost:8080/healthz
curl -sS http://localhost:8080/readyz
```

Expected responses:

```text
{"status":"ok"}
{"status":"ready"}
```

By default readiness returns `ready`. If `READINESS_REQUIRED=true`, readiness also checks configured local dependencies such as `DATA_PATH`.

### Persist local memory across restarts

Set `DATA_PATH` to store the local memory snapshot at `$DATA_PATH/memory.json`:

```bash
export API_TOKEN='dev-token'
export DATA_PATH='./var/workspace-brain'
export READINESS_REQUIRED='true'
go run ./cmd/workspace-brain
```

This is local JSON snapshot persistence, not a substitute for managed production storage or backups.

### Run the container image

Build the distroless non-root image:

```bash
docker build -t workspace-brain:local .
```

Run the JSON HTTP adapter:

```bash
docker run --rm -p 8080:8080 \
  -e API_TOKEN=dev-token \
  workspace-brain:local
```

Add a writable volume for local snapshot persistence:

```bash
docker run --rm -p 8080:8080 \
  -e API_TOKEN=dev-token \
  -e DATA_PATH=/data \
  -e READINESS_REQUIRED=true \
  -v workspace-brain-data:/data \
  workspace-brain:local
```

## Use the JSON HTTP adapter

The JSON adapter is the simplest non-Slack surface. It uses the same dispatcher and gateway as Slack.

Create a project as an admin:

```bash
curl -sS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"admin","roles":["admin"]},"text":"create Demo"}'
```

Ingest inline text through the source loader:

```bash
curl -sS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ingest text:workspace-brain%20keeps%20tenant%20knowledge%20isolated"}'
```

Check job status using the random hex job ID returned by the ingest response above:

```bash
curl -sS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"status job-616eec1b005fb0258433d73b843f8380"}'
```

Replace `job-616eec1b005fb0258433d73b843f8380` with the job ID from your ingest response. IDs are random hex generated by the gateway and cannot be predicted in advance.

Ask a grounded question:

```bash
curl -sS http://localhost:8080/api/commands \
  -H 'Authorization: Bearer dev-token' \
  -H 'Content-Type: application/json' \
  -d '{"binding_key":"web:space:S1","principal":{"source":"web","id":"U1","roles":["member"]},"text":"ask what does workspace-brain keep isolated?"}'
```

Expected response shape:

```json
{"visibility":"private","text":"..."}
```

The JSON API rejects unknown JSON fields, limits request bodies to 1 MiB, and returns surface-neutral response text with `private` visibility.

### Supported ingest sources

The default server gateway resolves source content before calling the memory core.

| Source shape | Example | Notes |
|---|---|---|
| Local file URL | `file:///tmp/demo.md` | Host must be empty or `localhost`; size limit applies. |
| HTTP(S) URL | `https://example.com/demo.txt` | Requires a 2xx response; timeout and size limit apply. |
| Text URI | `text:hello%20world` | URL-decoded into inline content. `text://hello` also works for simple host-shaped text. |
| Raw URI | `raw:hello%20world` | URL-decoded into inline content. `raw://hello` also works for simple host-shaped text. |
| Plain text fallback | `hello world` | Used when no URI scheme is present. |
| Inline metadata | `content`, `inline_content`, `inline`, `raw`, `text` | Used by lower-level gateway calls; the command API accepts the text command envelope. |

Default source bounds are 1 MiB and 5 seconds.

## Use the Slack adapter

Slack is one frontend adapter. It does not own the product architecture.

```bash
export SLACK_SIGNING_SECRET='replace-me'
export ADMIN_USERS='U123,U456'
export ADDR=':8080'
go run ./cmd/workspace-brain
```

Mounted Slack routes:

```text
POST /slack/commands
POST /slack/interactions
```

Generate a Slack app manifest endpoint by adding a public base URL:

```bash
export PUBLIC_BASE_URL='https://workspace-brain.example.com'
export SLACK_APP_NAME='Workspace Brain'
export COMMAND_NAME='brain'
```

Then fetch:

```text
GET /slack/manifest.json
```

Slack registration needs public HTTPS request URLs. The runtime validates that `COMMAND_NAME` has no leading slash or whitespace. It does not persist a command-name lock yet, so keep `COMMAND_NAME` stable after registering the Slack app.

## Configuration

Copy `.env.example` when you need a local reference, but remember the Go process reads environment variables from the process environment. It does not load `.env` files by itself.

| Variable | Required | Default | Used by | Notes |
|---|---:|---|---|---|
| `ADDR` | No | `:8080` | Server | Listen address. |
| `API_TOKEN` | Required unless Slack is configured | none | JSON HTTP API | Bearer token for `POST /api/commands`. Use a high-entropy secret outside local demos. |
| `SLACK_SIGNING_SECRET` | Required unless JSON API is configured | none | Slack adapter | Enables Slack signature verification and Slack routes. |
| `ADMIN_USERS` | No | empty | Slack adapter | Comma-separated Slack user IDs that receive the `admin` role. |
| `COMMAND_NAME` | No | `brain` | Slack adapter + manifest | Slash command name without the leading slash. Keep stable after Slack registration. |
| `PUBLIC_BASE_URL` | No | none | Slack manifest | Enables `GET /slack/manifest.json`; use an absolute public HTTPS URL for Slack registration. |
| `SLACK_APP_NAME` | No | `workspace-brain` | Slack manifest | Display name in generated Slack manifest JSON. |
| `DATA_PATH` | No | none | Local memory core | Stores the local memory snapshot at `$DATA_PATH/memory.json` and creates the directory with `0700` permissions. |
| `READINESS_REQUIRED` | No | `false` | Ops | When true, readiness checks configured local dependencies such as `DATA_PATH`. |
| `HTTP_READ_HEADER_TIMEOUT` | No | `5s` | Server | Go duration for request-header read timeout. |
| `HTTP_READ_TIMEOUT` | No | `15s` | Server | Go duration for full request read timeout. |
| `HTTP_WRITE_TIMEOUT` | No | `30s` | Server | Go duration for response write timeout. |
| `HTTP_IDLE_TIMEOUT` | No | `60s` | Server | Go duration for keep-alive idle timeout. |
| `HTTP_SHUTDOWN_TIMEOUT` | No | `10s` | Server | Go duration for graceful shutdown. |
| `OPENAI_BASE_URL` | No | `https://api.openai.com/v1` in the optional client | Optional provider seam | Read by `internal/core/ai.OpenAIConfigFromEnv`; not wired into the default server path. |
| `OPENAI_API_KEY` | No | none | Optional provider seam | Required only when constructing the optional OpenAI-compatible client. |
| `OPENAI_EMBEDDING_MODEL` | No | none | Optional provider seam | Required only for optional embedding calls. |
| `OPENAI_RESPONSE_MODEL` | No | none | Optional provider seam | Required only for optional response calls. |
| `OPENAI_ORG_ID` | No | none | Optional provider seam | Optional provider header. |
| `OPENAI_PROJECT_ID` | No | none | Optional provider seam | Optional provider header. |

Timeout values use Go duration syntax such as `5s` or `1m`.

## Command model

Every frontend adapter sends the same text command model to the dispatcher.

| Command | Role required | Result |
|---|---|---|
| `create <name>` | `admin` | Create a tenant project and bind the adapter-provided location. |
| `ingest <source-uri>` | `member` or `admin` | Load supported source content and index it in the tenant memory core. |
| `ask <question>` | `member` or `admin` | Return a grounded answer with sources/spans when evidence exists. |
| `discover <query>` | `member` or `admin` | Return metadata-only discovery results. |
| `status <job-id>` | `member` or `admin` | Read/reconcile tenant-scoped job state. |
| `admin on|off <tenant-id>` | `admin` | Toggle tenant serving state. |

## Repository layout

```text
cmd/workspace-brain/          # server and demo wiring
pkg/brainapi/                 # surface-neutral public contract types
internal/control/frontend/    # shared command dispatcher
internal/control/gateway/     # binding, auth, jobs, source loading, core orchestration
internal/control/httpapi/     # JSON HTTP frontend adapter
internal/control/jobs/        # control-plane job ledger
internal/control/ops/         # health/readiness endpoints
internal/control/slack/       # Slack command/interactivity/manifest adapter
internal/control/sources/     # bounded source loader for ingest metadata/content
internal/core/ai/             # optional local/OpenAI-compatible provider seams
internal/core/memory/         # local in-memory RAG core and JSON persistence
internal/platform/config/     # typed environment config
internal/platform/server/     # hardened HTTP server helper
docs/                         # architecture and deployment notes
```

The `omcustom/gpt-codex` directory contains agent definitions and instructions. The application development target is the repository root shown above.

## Quality gates

Run these before release:

```bash
go test ./...
go test -race ./...
go vet ./...
./scripts/check-coverage.sh coverage.out
```

`./scripts/check-coverage.sh` runs `go test ./... -coverprofile=coverage.out` and fails unless total coverage is exactly `100.0%`. CI also runs `govulncheck ./...`.

For container changes, also run:

```bash
docker build -t workspace-brain:local .
```

## Production adapter roadmap

The local runtime intentionally needs no managed services. Production deployments can replace local seams behind the same contracts:

- durable catalog, binding, and job persistence beyond the local JSON memory snapshot
- PostgreSQL + row-level security for tenant catalog/content storage
- Qdrant or equivalent tenant-scoped vector store
- RabbitMQ or equivalent retry/DLQ ingest workers
- optional OpenAI/OpenAI-compatible embedding and synthesis adapters with deterministic test seams
- hybrid retrieval/rerank and richer grounded synthesis
- Web/CLI/Admin frontend adapters
- audit log, backup, and observability exporters
- Compose/orchestrator manifests and runtime health checks around the committed non-root Dockerfile

## Documentation index

- [ARCHITECTURE.md](./ARCHITECTURE.md) — boundary-focused implementation architecture.
- [docs/deployment.md](./docs/deployment.md) — deployment modes, environment contract, and production readiness checklist.
- [docs/workspace-brain-control-plane-architecture.md](./docs/workspace-brain-control-plane-architecture.md) — control-plane design notes.
- [docs/workspace-brain-data-architecture.md](./docs/workspace-brain-data-architecture.md) — data-core design notes.
- [AGENTS.md](./AGENTS.md) — repository agent operating contract.

## License

Released under the [MIT License](./LICENSE).
