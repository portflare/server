# Portflare Server

This repository contains the Portflare server.

It is split out from the original monorepo so the server image, release pipeline, and documentation can live in a dedicated repository.

## What is here

- `cmd/reverse-server`: the server binary
- `internal/buildinfo`: version metadata helpers
- `Dockerfile`: production image for the server
- `examples/caddy/Caddyfile.example`: example front-proxy config
- `docs/local-testing.md`: local testing notes

## Build

```bash
make build
./dist/bin/reverse-server --version
```

## Run

```bash
export REVERSE_SERVER_LISTEN_ADDR=:8080
export REVERSE_BASE_DOMAIN=reverse.example.test
export REVERSE_STATE_PATH=./state.json
export REVERSE_ADMIN_USERS=admin@example.com
reverse-server
```

For local testing without an auth proxy:

```bash
export REVERSE_DISABLE_AUTH=true
export REVERSE_LOCAL_DEV_USER=alice-smith
export REVERSE_LOCAL_DEV_EMAIL=alice@example.com
```

## Docker

Build the server image:

```bash
docker build -t ghcr.io/portflare/server:dev .
```

Run it locally:

```bash
docker run --rm -p 8080:8080 \
  -e REVERSE_SERVER_LISTEN_ADDR=:8080 \
  -e REVERSE_BASE_DOMAIN=reverse.example.test \
  -e REVERSE_STATE_PATH=/data/state.json \
  -e REVERSE_DISABLE_AUTH=true \
  -e REVERSE_LOCAL_DEV_USER=alice-smith \
  -e REVERSE_LOCAL_DEV_EMAIL=alice@example.com \
  -v "$PWD/.data:/data" \
  ghcr.io/portflare/server:dev
```

## Client repo

The companion client now lives in a separate repository so it can publish its own container image independently.
