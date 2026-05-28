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
export PORTFLARE_SERVED_BY_ENABLED=true
export PORTFLARE_SERVED_BY_MODE=visible_and_headers
export PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED=true
export PORTFLARE_REPORT_ABUSE_ENABLED=true
export PORTFLARE_REPORT_ABUSE_CHALLENGE_MODE=off
portflare-server
```

## Served-by disclosure and abuse reports

Public deployments default to visible served-by disclosure plus response headers:

- `PORTFLARE_SERVED_BY_ENABLED=true`
- `PORTFLARE_SERVED_BY_MODE=visible_and_headers`
- `PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED=true`
- `PORTFLARE_REPORT_ABUSE_ENABLED=true`

The settings are copied into the state file on first run, displayed in `/admin` and `/api/admin/state`, and can be changed by an admin without restarting the server.

The public abuse report endpoint rate-limits reports by reporter IP, reported URL, reported host, route context, and a hash of the reporter contact email when one is supplied. Duplicate reports for the same URL are coalesced into the original case while preserving reporter count and category-count signals for triage. The HTML form includes honeypot and time-to-submit fields; API clients can omit the timing field.

Operators can require an external challenge hook for the report endpoint with:

```bash
export PORTFLARE_REPORT_ABUSE_CHALLENGE_MODE=captcha
```

Supported values are `off`, `captcha`, and `proof_of_work`. `captcha` requires a non-empty `challenge_token` field from an upstream captcha integration. `proof_of_work` requires a `pow_nonce` whose SHA-256 digest for `<reported-url>|<nonce>` starts with `0000`.

Self-hosted deployments that do not want HTML mutation can use:

```bash
export PORTFLARE_SERVED_BY_MODE=headers_only
export PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED=false
```

Private/self-hosted deployments can also disable all disclosure or abuse intake, but the admin UI warns because this removes public disclosure headers, visible notices, or report-abuse endpoints:

```bash
export PORTFLARE_SERVED_BY_ENABLED=false
export PORTFLARE_REPORT_ABUSE_ENABLED=false
```

## Readiness

The server exposes readiness and build metadata:

```bash
curl http://localhost:8080/readyz
```

The response includes the application name, effective version, commit, build time, Go version, and `runtime/debug.ReadBuildInfo` module/settings/dependency data.

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
