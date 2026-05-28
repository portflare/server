# Served-by security and privacy review

This review covers the served-by disclosure injected into proxied HTML, the fallback response headers, and the public abuse-report flow.

## CSP and injection behavior

Portflare does not relax, remove, or rewrite upstream `Content-Security-Policy` headers by default. The proxy forwards upstream CSP as received, then adds Portflare attribution headers and `Link` relations when the served-by policy is enabled.

The injected affordance is static HTML only. It has no inline JavaScript, no inline CSS, no event-handler attributes, and no external assets. It contains text plus normal links to `/about-portflare` and `/report-abuse`. This keeps the component usable on strict CSP pages without requiring `unsafe-inline` for scripts or styles.

Sandbox policies, unusual document structures, compressed responses, downloads, redirects, and non-HTML responses can prevent visible HTML injection or make it less prominent. In those cases the expected fallback is header-only disclosure:

- `X-Portflare-Served-By: Portflare`
- `X-Portflare-Learn-More: <absolute learn-more URL>`
- `X-Portflare-Report-Abuse: <absolute report URL>` when report intake is enabled
- `Link` headers for learn-more and report-abuse relations

Operators can set `PORTFLARE_SERVED_BY_MODE=headers_only` or disable HTML injection for compatibility-sensitive deployments.

## XSS and page-data access

The injected component does not read page content, inspect forms, access cookies, register event handlers, submit forms, or call network APIs. It is generated server-side from Portflare-controlled URLs and public route labels, with URL values escaped before insertion.

The component runs in the tenant page origin because it is inserted into the proxied HTML. That means tenant page scripts and CSS can interact with it like any other same-origin DOM. Treat it as a disclosure and reporting affordance, not as a security boundary.

## Clickjacking and tenant CSS hiding

Injected disclosure can be covered, hidden, restyled, or moved by tenant CSS or JavaScript. Portflare cannot make a same-origin injected banner tamper-proof without changing the serving model. Header fallback remains available for automated tools and security reporters even when the visible component is hidden.

Portflare does not add clickjacking headers to tenant app responses because doing so could break legitimate framed apps and would alter the upstream app's security policy. Operators should configure framing policy at the app or front-proxy layer when they own that policy decision.

## Report data minimization and retention guidance

Abuse reports store the metadata needed to investigate and deduplicate reports:

- reported URL, host, path, public route user/app labels when resolvable
- category, description, served-by context, reporter contact when provided
- reporter contact hash for rate limiting and duplicate analysis
- requester IP address, user agent, status, timestamps, reporter count, and category counts
- internal triage notes added by admins

Report records do not store request cookies, authorization headers, arbitrary browser headers, page contents, screenshots, form inputs from the reported app, or marketing click-tracking events.

Routine report records should be retained only for the operator's abuse-response window. A default operational target is 180 days for routine cases, then delete the record or truncate IP/user-agent metadata unless a legal hold, child-safety escalation, litigation need, or other safety obligation requires longer retention. Operators should document who can access reporter contact details and whether sanitized summaries may be shared with app owners.

## Privacy risks

Reported URLs and descriptions may contain personal data because reporters provide them directly. The public form tells reporters not to submit passwords, API keys, private tokens, or other secrets. Admins should avoid copying report details into less controlled systems unless needed for investigation or legal response.

Fallback headers expose only public app names and public user labels, not internal emails or API keys. The report link encodes the current URL so reporters can submit the route they are viewing; operators should treat report URL paths and query strings as potentially sensitive during triage.
