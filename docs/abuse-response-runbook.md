# Abuse response runbook

This runbook is for operators who receive reports through Portflare's public `Report abuse` flow. It is not legal advice. Replace the placeholders with the contacts, retention rules, and escalation obligations for your deployment.

Self-hosted disclaimer: for self-hosted Portflare instances, the instance operator is responsible for monitoring and responding to reports. Portflare software provides routing, disclosure, report intake, and admin workflow tools; it does not provide staffed moderation by default.

## Intake checklist

1. Open `/admin#abuse-reports` and review new reports at least once per operational day for private deployments, and more frequently for public deployments.
2. Check the reported URL, category, reporter notes, related reports, resolved user/app labels, app approval status, and app connection status.
3. Preserve the report ID and timestamps before contacting the owner or changing route state.
4. Classify severity with the matrix below.
5. Decide whether to disable the route before owner contact.

## Severity matrix

| Severity | Examples | Initial response target | Default action |
| --- | --- | --- | --- |
| S0 emergency | Imminent physical harm, child sexual abuse material, credible violence, active credential theft at scale | Immediate | Disable the route, preserve evidence, escalate to legal/safety contact |
| S1 high | Phishing, malware, financial scams, doxxing, unauthorized private data, repeat abuse | Same day | Disable or force visible/report controls while triaging; notify owner when safe |
| S2 medium | Spam, deceptive content, policy complaints, suspicious downloads without confirmation | 2 business days | Keep route visible, request owner response, monitor duplicates |
| S3 low | Mistaken reports, stale links, unclear trademark/copyright claims without required detail | 5 business days | Acknowledge if contact exists, request more information, close if unsupported |

If a report plausibly fits two severities, use the higher severity until triage reduces risk.

## Disable the route

Use the fastest available control for the deployment:

1. In `/admin`, find the reported user and app from the report detail.
2. If the app can be made non-public in the current build, disable or unapprove the app and record the action in admin notes.
3. If only served-by controls are available, set the app override to `force_visible` or enable `PORTFLARE_SERVED_BY_EMERGENCY_FORCE_VISIBLE=true` while using the front proxy, firewall, DNS, or load balancer to block the reported host/path.
4. If the route uses a dedicated public port, block that listener at the firewall or front proxy while preserving the state file.
5. Record who changed the route, when, what URL was affected, and why.

Do not delete the user, app, state file, logs, or report record as the first response. Removal can destroy evidence needed for investigation or legal response.

## Evidence preservation

Preserve only what is needed and keep it in an access-controlled location:

- abuse report ID, category, description, reporter contact if supplied, reporter IP/user-agent, created/updated timestamps
- reported URL, host, path, route user/app labels, app approval/connection state
- relevant server logs and reverse-proxy logs around the report window
- screenshots or page captures only when allowed by your policy and necessary for investigation
- admin actions, notes, owner notices, and legal escalation timestamps

Do not copy passwords, API keys, private tokens, visitor cookies, or unrelated user data into notes. If a report includes secrets, restrict access and follow the deployment's secret-handling policy.

Routine evidence should follow the retention target in `docs/served-by-security.md`: 180 days for routine reports, then delete or truncate IP/user-agent metadata unless legal hold, safety escalation, or litigation needs require longer retention.

## Owner notification

Notify the app owner/operator unless doing so would increase imminent harm, compromise an investigation, or conflict with legal instructions.

Owner notice should include:

- the public URL or route label under review
- the abuse category and a sanitized summary
- required action and deadline
- whether the route was disabled or left online during review
- what evidence the owner may provide
- appeal or correction path, if one exists for the deployment

Do not share reporter contact details with the owner unless your policy allows it and the reporter has consented or legal review approves it.

Template:

```text
Subject: Portflare abuse report for <route>

We received an abuse report for <route> categorized as <category>.
Summary: <sanitized summary>

Action taken: <route disabled / visible warning forced / no immediate action>
Required owner response by: <deadline>

Do not contact the reporter directly through Portflare unless we explicitly authorize sharing reporter contact details.
```

## Legal escalation

Use legal escalation for S0 reports, credible threats, child-safety reports, law-enforcement contact, court orders, subpoenas, sanctions/export-control concerns, or copyright/IP workflows that your organization requires legal to review.

LEGAL CONTACT PLACEHOLDER: `<legal-or-trust-safety-contact@example.com>`

LAW ENFORCEMENT PROCESS PLACEHOLDER: `<internal link or mailbox>`

COPYRIGHT/IP POLICY PLACEHOLDER: `<policy URL or mailbox>`

When legal escalation starts:

1. Mark the report status `escalated_legal`.
2. Preserve evidence and stop routine deletion for related records.
3. Limit internal discussion to approved channels.
4. Do not notify the app owner until legal approves notification if doing so may increase risk.

## Closure

Close the report only after the route action, owner communication, evidence decision, and escalation decision are recorded.

Use one of the existing statuses consistently:

- `new`
- `triaged_reviewing`
- `needs_more_info`
- `operator_notified`
- `mitigated`
- `rejected_no_violation`
- `escalated_legal`
- `closed`

Closure notes should be brief, factual, and safe to audit later.

