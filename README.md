# Portflare Server

This repository contains the Portflare server.

It is split out from the original monorepo so the server image, release pipeline, and documentation can live in a dedicated repository.

## What is here

- `cmd/portflare-server`: the server binary
- `internal/buildinfo`: version metadata helpers
- `Dockerfile`: production image for the server
- `examples/caddy/Caddyfile.example`: example front-proxy config
- `docs/local-testing.md`: local testing notes

## Build

```bash
make build
./dist/bin/portflare-server --version
```

## Run

```bash
export PORTFLARE_SERVER_LISTEN_ADDR=:8080
export PORTFLARE_BASE_DOMAIN=reverse.example.test
export PORTFLARE_STATE_PATH=./state.json
export PORTFLARE_ADMIN_USERS=admin@example.com
export PORTFLARE_TRAFFIC_STATS_INTERVAL=30s
portflare-server
```

## Traffic stats

The server tracks per-user/per-app request counters in interval buckets. The current implementation uses an in-memory `TrafficStore`, so stats reset on restart and can later be backed by Prometheus, SQLite, or Postgres.

```bash
curl /api/me/traffic
curl /api/me/traffic?app=web
curl /api/admin/traffic
curl /api/admin/traffic?user=alice-smith\&app=web
```

Empty buckets are not persisted or returned.

## CLI registration

When registration is open, unauthenticated clients can create a user and receive an API key:

```bash
curl -X POST http://localhost:8080/api/register \
  -H 'content-type: application/json' \
  -d '{"user_name":"alice","email":"alice@example.com"}'
```

The endpoint returns the API key once at creation time. Existing users receive `409 Conflict` so duplicate registration cannot leak a stored key.

For local testing without an auth proxy:

```bash
export PORTFLARE_DISABLE_AUTH=true
export PORTFLARE_LOCAL_DEV_USER=alice-smith
export PORTFLARE_LOCAL_DEV_EMAIL=alice@example.com
```

## Docker

Build the server image:

```bash
docker build -t ghcr.io/portflare/server:dev .
```

Run it locally:

```bash
docker run --rm -p 8080:8080 \
  -e PORTFLARE_SERVER_LISTEN_ADDR=:8080 \
  -e PORTFLARE_BASE_DOMAIN=reverse.example.test \
  -e PORTFLARE_STATE_PATH=/data/state.json \
  -e PORTFLARE_DISABLE_AUTH=true \
  -e PORTFLARE_LOCAL_DEV_USER=alice-smith \
  -e PORTFLARE_LOCAL_DEV_EMAIL=alice@example.com \
  -v "$PWD/.data:/data" \
  ghcr.io/portflare/server:dev
```

## Client repo

The companion client now lives in a separate repository so it can publish its own container image independently.
