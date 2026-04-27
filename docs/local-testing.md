# Portflare Server local testing

## Quick start

```bash
export REVERSE_SERVER_LISTEN_ADDR=:8080
export REVERSE_BASE_DOMAIN=reverse.example.test
export REVERSE_STATE_PATH=./state.json
export REVERSE_DISABLE_AUTH=true
export REVERSE_LOCAL_DEV_USER=alice-smith
export REVERSE_LOCAL_DEV_EMAIL=alice@example.com
reverse-server
```

Then open:

- `http://127.0.0.1:8080/admin`
- `http://127.0.0.1:8080/me`

## Notes

- in production, put the server behind a reverse proxy that injects `X-Auth-Request-User` and `X-Auth-Request-Email`
- clients authenticate with per-user keys beginning with `pf_`
- public routes follow `{app}-{user-label}.<base-domain>`
