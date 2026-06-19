<p align="center">
  <img src="docs/logo.svg" width="76" alt="">
</p>

<h1 align="center">Pulse</h1>

<p align="center">
  Uptime checks and a public status page, in one binary.<br>
  <sub>Go · standard library first · pure-Go SQLite · Prometheus metrics · ECS Fargate</sub>
</p>

<br>

Point it at a YAML file with the things you care about. It probes them on an
interval, opens an incident when something stays down, tells a webhook or a
Slack channel, and serves a status page with ninety days of history. No
database server, no cgo, no JavaScript framework: one static binary and one
SQLite file.

I built it to have a Go project where the interesting parts are the ones that
are easy to get subtly wrong: scheduling many probes without them bunching up
or piling up, deciding when a failure is an outage, and computing an uptime
figure that means what it says.

![The status page during an incident](docs/screenshots/incident-light.jpg)

<p align="center">
  <img src="docs/screenshots/status-dark.jpg" width="62%" alt="The status page in dark mode">
  <img src="docs/screenshots/mobile.jpg" width="24%" alt="The status page on a phone">
</p>

## Running it

Go 1.26 or newer.

```bash
go run ./cmd/pulse demo        # http://127.0.0.1:8080
```

The demo starts four fake services on loopback, fills ninety days of history
with a few scripted outages, and monitors them every five seconds. It prints a
`curl` command that takes one of them down; about ten seconds later an incident
opens, and it closes ten seconds after you bring the service back with the
same command and `/up/` instead of `/down/`.

For real checks:

```bash
go build -o pulse ./cmd/pulse
./pulse check -config pulse.yaml   # validate and exit
./pulse run   -config pulse.yaml
```

| | |
| --- | --- |
| `/` | the status page |
| `/api/status` | the same data as JSON |
| `/api/incidents` | incidents in the last ninety days |
| `/metrics` | Prometheus text format |
| `/healthz` | liveness |

## Configuration

Everything except `checks` has a default. Unknown keys are an error, so a typo
never silently falls back to a default, and every problem in the file is
reported at once.

```yaml
title: Northwind status
listen: ":8080"
database: /data/pulse.db
retention: 2160h        # 90 days

checks:
  - name: Public API
    group: Core
    kind: http          # http, tcp, dns or tls
    target: https://api.example.com/health
    interval: 30s
    timeout: 5s
    expect_status: [200]
    expect_contains: '"status":"ok"'
    fail_after: 3       # consecutive failures before an incident opens
    recover_after: 2    # consecutive successes before it resolves
    flap:               # also open when 6 of the last 10 probes failed
      window: 10
      failures: 6       # 0 turns it off

  - name: Certificate
    kind: tls
    target: example.com:443
    interval: 1h
    min_validity: 336h  # fail when it expires in less than 14 days

notifiers:
  - kind: webhook
    url: https://hooks.example.com/pulse
    secret: ${PULSE_WEBHOOK_SECRET}
  - kind: slack
    url: ${SLACK_WEBHOOK_URL}
```

Notifier `url` and `secret` may be `${NAME}`, read from the environment, and a
reference to a variable that is unset or empty stops Pulse at startup: a
missing secret would otherwise send every webhook unsigned. Nothing else is
expanded, so `expect_contains: "$5.00"` means what it says. The timeout is
capped at 0.8 of the interval, because the next tick can come as early as 0.9
of it. When `PULSE_CONFIG` is set, its contents are used instead of the file,
which is how the container gets its config from a secret store. A fuller
example is in `examples/pulse.yaml`.

## How checks and incidents work

**Scheduling.** Each check waits a random fraction of its interval before its
first run, so forty checks on a 30-second interval do not all fire in the same
millisecond after a restart, and every interval after that gets up to ten
percent of jitter so checks that happen to line up drift apart. Probes run on a
bounded worker pool; when it is full the scheduler waits, which is the
back-pressure a slow network should cause. A check never overlaps itself: if a
probe is still running when the next one is due, the tick is counted in
`pulse_skipped_ticks_total` and runs the moment that probe returns, so a
hanging target does not leave holes in its own downtime.

**Probes.** Every probe runs under its own timeout and reports a short reason:
`timeout`, `connection refused`, `status 503`, `certificate expires in 9 days`.
HTTP probes follow up to five redirects, open a fresh connection each time,
and search at most the first megabyte for `expect_contains`. A reason never
includes a URL: a redirect target is chosen by the site being probed, and it
would otherwise travel into Slack. The TLS check fails on the first
certificate in the chain to expire, not just the leaf.

**Incidents.** An incident opens after `fail_after` consecutive failures and is
dated from the first of them, because that is when the outage started. It
resolves after `recover_after` consecutive successes, dated from the first
success. One blip below the threshold never becomes an incident, and one good
probe in the middle of an outage does not end it. A service that flaps, failing
two probes in three but never three in a row, opens one too, through the
`flap` window. A gap longer than `max_gap` between two results breaks every
streak: failures from before Pulse stopped watching say nothing about after.

**Restarts.** On startup Pulse replays the last few results of every check,
as long as they are newer than `max_gap`. A
restart in the middle of an outage keeps the incident it already had, a
failure streak that had not crossed the threshold yet is remembered, and an
incident a crash left half-written (results stored, incident row not) is
repaired. A probe cut short by the shutdown itself is not recorded, so a deploy
does not paint a red mark on every service.

**Uptime.** Each probe stands for the time until the next one, so a minute of
failures probed every five seconds weighs one minute, not twelve times as much
as an hour probed every ten minutes. One probe never stands for more than
`max_gap` (twice the interval by default): past that, Pulse was not looking,
and time nobody looked at is no data, neither uptime nor downtime. The
percentage is truncated, never rounded, so 99.996% reads 99.99% and a page with
an incident on it never claims 100%.

**Latency.** p95 is nearest-rank over successful probes only. A timeout already
counts as downtime; letting it into the latency figure would count it twice.

## Notifications

Webhooks are JSON, delivered in the background with retries, from a bounded
queue so a dead receiver can never delay a probe. When a secret is set, each
request carries a signature in the same shape Stripe uses:

```
Pulse-Signature: t=1790000000,v1=5f3c...e1
```

where `v1` is the hex HMAC-SHA256 of `"<t>.<body>"`. Verifying it in Python:

```python
import hashlib, hmac, time

def verify(secret: bytes, header: str, body: bytes, tolerance=300) -> bool:
    parts = dict(p.split("=", 1) for p in header.split(","))
    expected = hmac.new(secret, f"{parts['t']}.".encode() + body, hashlib.sha256).hexdigest()
    fresh = abs(time.time() - int(parts["t"])) <= tolerance
    return fresh and hmac.compare_digest(expected, parts["v1"])
```

`kind: slack` posts a one-line message to anything that accepts Slack's
`{"text": ...}` shape, with `&`, `<` and `>` escaped the way Slack asks, so a
check name or reason cannot ping `@channel` or dress up a link.

Every notifier has its own queue and goroutine, so a Slack URL that hangs does
not hold back the webhook. Network errors, 5xx, 408 and 429 are retried with
backoff; any other 4xx (a revoked URL, a receiver that rejects the payload) is
logged once and not retried. On shutdown the queues drain until the shutdown
deadline, and anything still waiting after it is logged and dropped.

## Deploying to AWS

`infra/` is Terraform (it validates with OpenTofu too) for one Fargate task
behind an Application Load Balancer, with the SQLite file on an encrypted EFS
volume and the config in SSM Parameter Store.

```bash
cd infra
terraform init
terraform apply \
  -var vpc_id=vpc-... \
  -var 'public_subnet_ids=["subnet-a","subnet-b"]' \
  -var 'private_subnet_ids=["subnet-c","subnet-d"]' \
  -var certificate_arn=arn:aws:acm:... \
  -var image=<account>.dkr.ecr.<region>.amazonaws.com/pulse:1.0.0 \
  -var public_url=https://status.example.com \
  -var 'secret_env={PULSE_WEBHOOK_SECRET="...", SLACK_WEBHOOK_URL="https://hooks.slack.com/..."}'
```

`secret_env` holds whatever the config references as `${NAME}`. Each entry
becomes a SecureString parameter and an environment variable in the task.

It runs exactly one task, on purpose. SQLite wants a single writer, and a
monitor that runs twice sends every alert twice, so deployments stop the old
task before starting the new one. The page is unavailable while that happens,
which is a few seconds because the target group skips the default five-minute
drain. The image is `distroless/static` running as non-root, built by the
`Dockerfile`; `.goreleaser.yaml` builds release binaries.

## Things worth opening

**`internal/scheduler/scheduler.go`.** Start offsets, jitter and the no-overlap
rule, driven by a `clock.Clock` so the tests move time by hand instead of
sleeping.

**`internal/store/store.go`.** The time-weighted uptime is a `LEAD()` window
function with a cap, in one query. Writes go through one lock, reads run
concurrently under WAL, and pruning deletes in batches so it never holds that
lock for long.

**`internal/incident/incident.go`.** Thirty lines that decide what an outage is.
The test is a table of strings like `"---+-++"`.

**`internal/monitor/monitor.go`.** The replay on startup, and the check that
keeps a cancelled probe out of the history.

**`internal/web/`.** A server-rendered page, `html/template` escaping, a strict
Content Security Policy (the page loads nothing from anywhere else), and the
Prometheus text format written by hand in a dozen lines.

## Tests

```bash
go test -race ./...
go test -run xxx -bench . ./internal/pool
```

61 tests (69 with subtests), all with the race detector on. The probes run
against `httptest` servers and real sockets, including a TLS server whose
certificate is checked for trust and expiry, and a chain whose intermediate
expires before the leaf. A site that redirects forever to a URL full of Slack
markup checks that the reason never carries it. The scheduler and the monitor run
on a fake clock. The store is tested against real SQLite files: the uptime
weighting, gaps, the p95 rank, batched pruning, and eight writers with four
readers at once. The monitor tests restart on the same database mid-outage,
after a simulated crash, and after three days of Pulse being down. The
incident tests cover flapping and gaps; the notifier tests cover escaping,
which statuses are retried, and one hung receiver next to a healthy one. Webhook signatures are checked against a vector
computed outside Go. The page test feeds a check named `<script>` through the
template.

## Layout

```
cmd/pulse/          run, check, demo
internal/
  config/           pulse.yaml, strict decoding, defaults
  check/            http, tcp, dns, tls probes
  clock/            real and fake time
  pool/             bounded worker pool
  scheduler/        start offsets, jitter, no overlap
  store/            SQLite: results, incidents, uptime, retention
  incident/         failure streaks to incidents
  monitor/          wires the above, replay on startup
  notify/           signed webhooks, Slack, background delivery
  web/              status page, JSON, /metrics
  demo/             fake services and ninety days of history
infra/              ECS Fargate, EFS, ALB
```

## What's missing

- One region. Probing from a single place cannot tell "the service is down"
  from "the path from here is down"; multi-region would need agreement between
  probes before opening an incident.
- No maintenance windows. Planned downtime counts as downtime.
- No auth on the page. It is meant to be public; anything private should not
  be a check here.
- The Docker image and the Terraform have been validated, not run: I have not
  built the image or applied the stack against a real account.
