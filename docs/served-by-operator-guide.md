# Served-by operator guide

This guide documents the served-by disclosure, fallback headers, report-abuse entry points, and compatibility limits for Portflare public deployments.

## Default public deployment recommendation

Public deployments should keep visible disclosure, fallback headers, and abuse intake enabled:

```bash
export PORTFLARE_SERVED_BY_ENABLED=true
export PORTFLARE_SERVED_BY_MODE=visible_and_headers
export PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED=true
export PORTFLARE_REPORT_ABUSE_ENABLED=true
export PORTFLARE_REPORT_ABUSE_CHALLENGE_MODE=off
export PORTFLARE_SERVED_BY_APP_DISABLE_ALLOWED=false
export PORTFLARE_SERVED_BY_EMERGENCY_FORCE_VISIBLE=false
```

These are the server defaults for new state. They make eligible public HTML pages show `Served by Portflare`, `Learn more`, and `Report abuse`, while non-HTML or unsafe-to-mutate responses receive attribution and report headers.

Self-hosted private deployments can weaken or disable disclosure for internal compatibility, but the operator must document why, monitor reports, and understand that disabling report intake removes the public abuse path for that instance.

## Modes and defaults

`PORTFLARE_SERVED_BY_ENABLED` controls whether Portflare adds served-by disclosure at all. Default: `true`.

`PORTFLARE_SERVED_BY_MODE` accepts:

- `visible_and_headers`: default. Inject the visible affordance into eligible HTML and add fallback headers.
- `headers_only`: add fallback headers, but do not inject visible HTML.

`disabled` is not a global mode string. Global disable is represented by `PORTFLARE_SERVED_BY_ENABLED=false`, and per-app disable is represented by the `disabled` override when the operator has explicitly allowed it.

`PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED` controls HTML rewriting inside `visible_and_headers`. Default: `true`. If set to `false`, effective behavior is headers-only for visible disclosure.

`PORTFLARE_REPORT_ABUSE_ENABLED` controls public report links and `/report-abuse` intake. Default: `true`.

`PORTFLARE_REPORT_ABUSE_CHALLENGE_MODE` accepts `off`, `captcha`, or `proof_of_work`. Default: `off`.

`PORTFLARE_SERVED_BY_APP_DISABLE_ALLOWED` controls whether an app-level `disabled` override can suppress served-by disclosure. Default: `false`.

`PORTFLARE_SERVED_BY_EMERGENCY_FORCE_VISIBLE` forces visible disclosure and report links for every app, overriding weaker app settings during an incident. Default: `false`.

## Admin settings and persisted state

On first run, the environment defaults are copied into the JSON state file. After state exists, the admin UI and admin API represent the current policy.

Operators can inspect and change settings in:

- `/admin`
- `/api/admin/state`

The admin page warns when an operator disables served-by, selects headers-only behavior, disables HTML injection, disables report abuse, allows per-app disable, or enables emergency force-visible mode.

## Per-app overrides

Admin users can set per-app overrides through `/api/admin/app-served-by-override` or the app table in `/admin`.

Supported per-app overrides:

- `inherit`: use the global served-by policy.
- `force_visible`: use `visible_and_headers` for this app even when the global mode is weaker.
- `headers_only`: keep attribution/report headers but skip visible injection.
- `disabled`: suppress served-by disclosure for this app only when `PORTFLARE_SERVED_BY_APP_DISABLE_ALLOWED=true` or the matching admin setting is enabled.

Every weakening override should include a reason. Use `force_visible` or `PORTFLARE_SERVED_BY_EMERGENCY_FORCE_VISIBLE=true` when abuse response requires visible attribution across affected apps.

## HTML rewriting behavior

Portflare only injects the visible affordance into responses that are safe to mutate:

- request method is `GET`
- upstream status is 2xx
- response can have a body
- `Content-Type` is `text/html` or `application/xhtml+xml`
- response is not an attachment
- response has no unsafe `Content-Encoding`
- response is not a protocol upgrade
- response body is non-empty and valid base64 from the tunnel client

When HTML rewriting occurs, Portflare inserts static markup before `</body>`, before `</html>`, or appends it as a fallback. The markup contains no script, inline style, image, iframe, form, or external asset.

Because the payload changed, Portflare recalculates `Content-Length` and removes stale payload validators such as `ETag`, `Last-Modified`, `Content-MD5`, and `Digest`.

## Compatibility caveats

Compression: responses with non-identity `Content-Encoding` such as gzip or br are not rewritten. Portflare falls back to headers because it does not decode and re-encode compressed upstream HTML.

CSP: Portflare preserves upstream `Content-Security-Policy` headers. It does not relax CSP to make the notice more prominent. The injected markup is intentionally static so it can render under strict CSP, but app CSS or layout can still affect visibility.

Streaming: the current tunnel response model buffers the upstream response body before writing it to the visitor. Served-by injection is not a streaming transformation and should not be presented as one.

Single-page apps: single-page apps can receive the visible notice in the initial HTML shell when eligible. Later client-side route changes will not trigger another injection, and app code can rerender around the notice.

Binary responses: images, archives, PDFs, media, JSON APIs, downloads, websocket upgrades, and other binary responses are not rewritten. They receive fallback headers when served-by is enabled and the response is not an upgrade.

HTML structure: unusual documents without a normal body can still receive an appended notice, but page layout may make it less visible. The notice is an attribution and reporting aid, not a tamper-proof security badge.

Tenant control: the notice runs in the app's origin. Tenant CSS or JavaScript can hide, restyle, or move it. Operators should rely on a combination of visible disclosure, fallback headers, admin review, and abuse response.

## Visitor-facing copy policy

Use neutral language:

- Say `Served by Portflare`.
- Say `Learn more`.
- Say `Report abuse`.

Do not describe proxied apps as protected, verified, trusted, certified, scanned, or endorsed by Portflare unless a separate product and legal review has approved that claim.

The learn-more and report pages must explain that Portflare routes traffic for independently operated apps. For self-hosted instances, the self-hosted operator is responsible for monitoring and responding to reports unless a managed-service agreement says otherwise.
