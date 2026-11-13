# How to route Alertmanager alerts to agents

An alert receiver turns a Prometheus Alertmanager notification into a run.
Alertmanager posts a notification to the receiver's webhook. Haliphron starts
one agent run under the role you chose, with your instruction and the
notification's firing alerts in the prompt. If the same alert keeps firing, it
does not start a run per notification. Once an alert has started a run, it is
ignored for the receiver's throttle window, one hour unless you change it.

One notification is one run. Alertmanager has already grouped the alerts by
your route's `group_by`, and that group is the unit of work. Haliphron does not
regroup the alerts.

## Before you start

- A role for the runs. The role is required. It decides what a run may do, and
  **anyone holding the receiver's credential can start runs under it**. Give it
  only what investigating an alert needs. See [Define a role](define-a-role.md).
- An admin token. Creating a receiver mints a credential, which is why it needs
  `admin`.
- An Alertmanager that can reach the public API over HTTPS. The backend must
  serve the API, so `backend.mode` is `all` or `api`.

## Create the receiver

In the UI, open **Alerts → New receiver** and fill in the following fields:

- **Name**, for example `on-call-triage`.
- **Throttle**, how long an alert that started a run is ignored. The presets
  are off, 15 min, 1 h, 4 h and 24 h; the most it can be is a week. `0` turns
  suppression off: every notification starts a run.
- **Prompt**, the instruction. The alerts are appended after it, so write it
  as if they follow. For example: "Investigate the firing alerts below. Find
  the likely cause and say what you found; if it is in this repository, open a
  pull request with the fix."
- **Role**, and the repository, budget and timeout, exactly as for a run.

On save, the template is checked by the same rules as a run. A role that does
not exist is refused there, not at the first page in the middle of the night.

The dialog then shows the receiver's credential, `hla_…`, **once**. Only its
digest is stored. It also shows the Alertmanager configuration, filled in.

Over REST:

```sh
curl -sS -X POST "$HALIPHRON/api/v1/alert-receivers" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "name": "on-call-triage",
    "throttle_seconds": 3600,
    "run": {
      "role": "sre",
      "repo": "https://github.com/acme/checkout",
      "max_cost_usd": "3",
      "prompt": "Investigate the firing alerts below and say what you found."
    }
  }'
```

The answer carries `id`, `webhook_path` and `token`.

## Configure Alertmanager

Put the credential in a file Alertmanager can read, for example from a
Kubernetes Secret mounted at `/etc/alertmanager/haliphron-token`. Then add a
receiver and route alerts to it:

```yaml
route:
  routes:
    - matchers: [ 'severity="page"' ]
      receiver: haliphron-on-call-triage
      group_by: [ alertname, namespace ]
      group_interval: 5m
      repeat_interval: 4h

receivers:
  - name: haliphron-on-call-triage
    webhook_configs:
      - url: https://haliphron.example.com/api/v1/alert-receivers/01J.../alerts
        send_resolved: false
        max_alerts: 50
        http_config:
          authorization:
            type: Bearer
            credentials_file: /etc/alertmanager/haliphron-token
```

- **`send_resolved: false`**. Resolved alerts start nothing. They are recorded
  and dropped, so sending them only adds deliveries to the log.
- **`max_alerts`** keeps a notification inside the 1 MiB and 1000-alert bounds,
  and keeps the prompt focused. Haliphron tells the agent when Alertmanager
  truncated a group.
- **`group_by`, `group_interval`, `repeat_interval`** are your main controls
  for how many runs you get. A storm across fifty groups is fifty runs.
  Haliphron has no per-receiver rate ceiling yet.

## What a delivery does

| Outcome | Meaning |
|---|---|
| `submitted` | A run started with the alerts no earlier run is still handling |
| `throttled` | Every firing alert of the notification was suppressed; nothing started |
| `resolved` | The notification had no firing alert |
| `refused` | Admission refused the template, for example because the role was deleted. The reason is on the receiver |

All four are answered `200`, so Alertmanager does not retry them. A wrong
credential is `401`, a disabled receiver `404`, an unreadable payload `400`,
and a store failure `503`. Alertmanager retries the `503`, and a retry of a
notification that already started a run is throttled.

A group that gains an alert starts a run for the new alert only. Its prompt
says which run is handling the others.

## Watch and adjust

The receiver's page shows:

- **Notifications**: every delivery of the last 14 days, with its outcome, the
  fingerprints it left out, and the run it started.
- **Suppressed now**: each suppressed alert, until when, and which run handled
  it. **Clear** lifts one suppression and **Clear all** lifts every one; the
  next notification carrying the alert starts a run.
- **Runs**: every run the receiver started.

Changing the throttle applies to alerts claimed afterwards. Alerts already
suppressed keep their window.

`haliphron_alert_deliveries_24h{outcome="refused"}` is the metric to alert on:
it means a receiver's template no longer admits.

## Rotate or remove the credential

**Rotate credential** on the receiver's page, or `POST
/api/v1/alert-receivers/{id}/rotate-token`, mints a new credential and shows it
once. The old one stops working at once, so update Alertmanager's file right
after.

**Disable** pauses the receiver, and its deliveries are answered `404`.
**Delete** removes the receiver with its suppressions and log. The runs it
started are kept.

## See also

- [REST API: alert receivers](../reference/rest-api.md#alert-receivers)
- [About alert throttling](../explanation/alert-throttling.md)
