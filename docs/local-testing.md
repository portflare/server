# Portflare Server local testing

## Quick start

```bash
export PORTFLARE_SERVER_LISTEN_ADDR=:8080
export PORTFLARE_BASE_DOMAIN=reverse.example.test
export PORTFLARE_STATE_PATH=./state.json
export PORTFLARE_DISABLE_AUTH=true
export PORTFLARE_LOCAL_DEV_USER=alice-smith
export PORTFLARE_LOCAL_DEV_EMAIL=alice@example.com
portflare-server
```

Then open:

- `http://127.0.0.1:8080/admin`
- `http://127.0.0.1:8080/me`

## Notes

- in production, put the server behind a reverse proxy that injects `X-Auth-Request-User` and `X-Auth-Request-Email`
- clients authenticate with per-user keys beginning with `pf_`
- public routes follow `{app}-{user-label}.<base-domain>`
