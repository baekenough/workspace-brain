# Local Development Guide

This guide explains how to run the workspace-brain dependency stack locally using Docker Compose and then start the application against it.

## Prerequisites

- Docker Engine 24+ with the Compose plugin (`docker compose version`)
- Go 1.25+ for building and running the application binary
- `make` (standard on macOS and Linux)

## Quick Start

### 1. Copy the environment template

```bash
cp .env.example .env
```

The defaults in `.env.example` match the local Compose stack exactly — no edits are needed for a first run. Review the `API_TOKEN` value and set a stronger secret if you are exposing the service on your network.

### 2. Start the dependency stack

```bash
make dev-up
```

This runs `docker compose up -d` which starts three containers in the background:

| Service    | Image                   | Port(s)                              |
|------------|-------------------------|--------------------------------------|
| postgres   | postgres:16-alpine      | `5432`                               |
| qdrant     | qdrant/qdrant:latest    | `6333` (REST), `6334` (gRPC)         |
| rabbitmq   | rabbitmq:3-management   | `5672` (AMQP), `15672` (management)  |

All three services have healthchecks. They are considered ready when their healthchecks pass. You can verify:

```bash
docker compose ps
```

### 3. Set environment variables

Export the service-connection variables into your shell (or load your `.env` file using a tool such as `direnv`):

```bash
export POSTGRES_DSN="postgres://brain:brain_local@localhost:5432/brain?sslmode=disable"
export QDRANT_URL="http://localhost:6333"
export RABBITMQ_URL="amqp://brain:brain_local@localhost:5672/brain"
export API_TOKEN="dev-token"
```

These defaults match the credentials defined in `docker-compose.yml`.

### 4. Run the application

```bash
go run ./cmd/workspace-brain
```

The server starts on `:8080` by default. Verify liveness:

```bash
curl http://localhost:8080/livez
curl http://localhost:8080/readyz
```

Send a test command:

```bash
curl -s -X POST http://localhost:8080/api/commands \
  -H "Authorization: Bearer dev-token" \
  -H "Content-Type: application/json" \
  -d '{"command":"ping"}'
```

## Optional: run the app in Docker

The `app` profile in `docker-compose.yml` builds the workspace-brain image from the local `Dockerfile` and wires it to the dependency services automatically.

```bash
# Start deps + app together
docker compose --profile app up -d

# Or bring up the stack including the app build
docker compose --profile app up --build -d
```

The `app` service waits for all three dependency healthchecks before starting.

## Viewing logs

```bash
# All containers
make dev-logs

# Single service
docker compose logs -f postgres
docker compose logs -f qdrant
docker compose logs -f rabbitmq
```

## RabbitMQ management UI

Browse to [http://localhost:15672](http://localhost:15672) and log in with:

- **Username:** `brain`
- **Password:** `brain_local`

The default virtual host is `brain`.

## Tearing down

Stop containers and remove networks (volumes are preserved):

```bash
make dev-down
```

Remove containers **and** all local data volumes:

```bash
docker compose down -v
```

## Environment Variables Reference

The following variables are used to connect the app to the local dependency stack. They are added to `.env.example` with safe local-dev defaults.

| Variable           | Default                                                     | Description                              |
|--------------------|-------------------------------------------------------------|------------------------------------------|
| `POSTGRES_DSN`     | `postgres://brain:brain_local@localhost:5432/brain?sslmode=disable` | Full PostgreSQL connection DSN      |
| `POSTGRES_HOST`    | `localhost`                                                 | PostgreSQL host (informational)          |
| `POSTGRES_PORT`    | `5432`                                                      | PostgreSQL port (informational)          |
| `POSTGRES_USER`    | `brain`                                                     | PostgreSQL user (informational)          |
| `POSTGRES_PASSWORD`| `brain_local`                                               | PostgreSQL password (informational)      |
| `POSTGRES_DB`      | `brain`                                                     | PostgreSQL database name (informational) |
| `QDRANT_URL`       | `http://localhost:6333`                                     | Qdrant REST API base URL                 |
| `RABBITMQ_URL`     | `amqp://brain:brain_local@localhost:5672/brain`             | RabbitMQ AMQP connection URL             |

> **Note:** `POSTGRES_HOST` / `POSTGRES_PORT` / `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` are provided as convenience references. The application code in `internal/core/postgres` consumes `POSTGRES_DSN` directly.

> **Note:** RabbitMQ is a planned ingest backend. `RABBITMQ_URL` is included here for forward compatibility; it is not yet read by the default server path.

## Makefile Targets

| Target          | Description                                                 |
|-----------------|-------------------------------------------------------------|
| `make dev-up`   | Start postgres, qdrant, rabbitmq in background              |
| `make dev-down` | Stop and remove containers and networks                     |
| `make dev-logs` | Tail logs from all dependency containers                    |
| `make build`    | Build the workspace-brain binary                            |
| `make test`     | Run unit tests                                              |
| `make docker-build` | Build the local Docker image                            |
