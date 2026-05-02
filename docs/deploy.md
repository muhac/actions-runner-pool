# Deployment guide

This walks through a real deployment of gharp on a single host: a
container, a public HTTPS URL, and the GitHub App setup. The README
quick-start uses `docker run` directly; this guide covers
`docker compose`, public-URL options (Cloudflare Tunnel / ngrok /
Tailscale Funnel), running from source, and day-2 operations.

For the full env-variable reference, see [configuration.md](configuration.md).

## Requirements

- A host with Docker (any modern Linux; tested on amd64 + arm64).
- Access to the host's Docker daemon socket (`/var/run/docker.sock`).
- A public HTTPS URL that GitHub can reach.
- A GitHub account or organization where you'll install the App.

> ⚠️ **Untrusted code**: gharp runs CI jobs from any repo where the App
> is installed, in containers that share the host Docker socket. Run
> only on a dedicated VM, cloud instance, or homelab node — not on a
> machine that holds secrets you wouldn't paste into a workflow. Public
> repos are blocked by default; prefer `REPO_ALLOWLIST=owner/repo` over
> `ALLOW_PUBLIC_REPOS=true` when you need to allow selected public repos.

## 1. Get the code (or skip — use the published image)

The fastest path is the pre-built multi-arch image
[`muhac/gharp`](https://hub.docker.com/r/muhac/gharp), shown in the
README quick-start. Use that if you don't need to modify the code.

To run from source (custom image, local development, or a fork):

```bash
git clone https://github.com/muhac/actions-runner-pool
cd actions-runner-pool
cp .env.example .env
```

Edit `.env` and set `BASE_URL`. Leave the rest defaulted for now —
[configuration.md](configuration.md) lists every knob. From here,
`docker compose up -d` (the bundled `docker-compose.yml`) builds the
image locally and runs it with the same volumes the README example
uses.

## 2. Provide a public HTTPS URL

GitHub needs to POST webhooks to `${BASE_URL}/github/webhook`. Pick
based on whether the URL needs to survive restarts.

### Production — stable hostname

#### Cloudflare Tunnel (named, with DNS)

```bash
cloudflared tunnel create gharp
cloudflared tunnel route dns gharp gharp.example.com
# In ~/.cloudflared/config.yml, route gharp.example.com → http://localhost:8080
cloudflared tunnel run gharp
```

`BASE_URL=https://gharp.example.com`. Stable across restarts; survives
container/host reboots.

#### Tailscale Funnel

Expose port 8080 via `tailscale serve` + `tailscale funnel`. The
hostname is your tailnet's MagicDNS name and is stable.

#### Cloud-hosted (VPS, EC2, Hetzner, etc.)

Run gharp on a public host with TLS terminated by Caddy / nginx /
Traefik in front, and point `BASE_URL` at the public DNS name. No
tunnel needed.

### Local dev — ephemeral hostname

These give you a public URL in seconds but the hostname changes every
restart. Each change requires re-running `/setup` (fresh GitHub App)
because `BASE_URL` is sticky — see "BASE_URL drift" in
[`configuration.md`](configuration.md).

#### Cloudflare quick tunnel

```bash
cloudflared tunnel --url http://localhost:8080
# Prints a https://<random>.trycloudflare.com URL.
```

#### ngrok

```bash
ngrok http 8080
# Note the https://<random>.ngrok-free.app URL.
```

## 3. Boot the stack

```bash
docker compose up -d
docker compose logs -f gharp
```

You should see:

```text
gharp listening addr=:8080 base_url=https://gharp.example.com
```

The default `docker-compose.yml` mounts:

| Path | Purpose |
| --- | --- |
| `/var/run/docker.sock` | Lets gharp `docker run` runner containers on the host. |
| `gharp-data` (volume) → `/data` | Holds the SQLite DB across restarts. |
| `/tmp/gharp` (host) → `/tmp/gharp` (container) | Workdir tree. Mounted at the same path on both sides so `-v /tmp/gharp/...` in `RUNNER_COMMAND` resolves identically from gharp's view and the host daemon's view. |

## 4. Create the GitHub App

Open `${BASE_URL}/setup` and click **Create GitHub App**. gharp drives
the GitHub App Manifest flow: GitHub generates the App, redirects back
to `${BASE_URL}/github/app/callback?code=...`, and gharp persists the
private key, webhook secret, and client secret in `app_config`. Then
click the install link gharp renders, choose the repos (or "All
repositories"), and submit.

For the underlying flow and the security caveats, see
[`architecture.md` § 5](architecture.md).

You're done. From this point, every `workflow_job` whose `runs-on` set
is satisfiable from `RUNNER_LABELS` will get a fresh runner.

## 5. Verify end-to-end

In a test repo where the App is installed:

```yaml
# .github/workflows/smoke.yml
name: smoke
on: [push, workflow_dispatch]
jobs:
  hello:
    runs-on: [self-hosted]
    steps:
      - run: echo "hello from $(hostname)"
```

Push it. On the host, you should see:

```bash
docker logs gharp
# dispatch: runner launched job_id=... container=gharp-...-...

docker ps
# ... myoung34/github-runner:latest ... gharp-<job_id>-<hash>
```

The runner container should disappear within a few seconds of the job
finishing in the GitHub UI (it's `--rm` + `EPHEMERAL=1`).

## Operations

### Upgrades

Pull the new image and recreate:

```bash
docker compose pull
docker compose up -d
```

The SQLite DB on the `gharp-data` volume is preserved. The schema
uses `CREATE TABLE IF NOT EXISTS` and runs at every startup; there's
no migration framework yet, so additive schema changes are safe but
column drops/renames would require manual SQL.

### Backup

The only stateful piece is the `gharp-data` volume (a single SQLite
file at `/data/gharp.db`). Snapshot the volume with `docker run --rm
-v gharp-data:/src -v /backup:/dst alpine tar -C /src -czf /dst/gharp-$(date +%F).tgz .`,
or use `sqlite3 .backup` from a sidecar container that has the
`sqlite` package installed (the gharp image itself does not).

### Rotating credentials

`BASE_URL`, the App's private key, and the webhook secret are baked
into the App at `/setup` time. There's no rotation flow in v1 — to
change any of them, delete the App on GitHub, wipe the gharp volume
(`docker volume rm gharp-data`), and re-run `/setup`. See
[`architecture.md` § 6](architecture.md) for the design rationale.

### Troubleshooting

- **No webhooks landing.** Confirm `BASE_URL` is reachable from the
  public internet (`curl ${BASE_URL}/setup` from outside your network).
  Check the App's "Recent Deliveries" page on GitHub for HTTP errors.
- **Webhook 401.** The signing secret in `app_config` doesn't match the
  one GitHub holds. This usually means you swapped the DB but kept the
  old App, or vice versa. Re-run `/setup`.
- **Runners start but never pick up the job.** Check `RUNNER_LABELS` —
  a job is accepted only if every label in its `runs-on` set is
  satisfiable from `RUNNER_LABELS` (or is the implicit `self-hosted`).
- **Public repo jobs are ignored.** This is the default safety guard.
  Set `REPO_ALLOWLIST=owner/repo` for selected public repos, or
  `ALLOW_PUBLIC_REPOS=true` to allow all public repos where the App is
  installed.
- **`BASE_URL drift` warning at startup.** Your `BASE_URL` env differs
  from the URL stored in `app_config`. Either revert the env or re-run
  `/setup`. See `configuration.md`.
- **Runners pile up after jobs finish.** Should not happen with the
  default `--rm` command. If you removed `--rm` in a custom
  `RUNNER_COMMAND`, restore it or add your own GC. The reconciler's
  `RUNNER_MAX_LIFETIME` sweep (default 2h) is a backstop, not a
  replacement.
- **Cap appears stuck (`active runners` count won't drop).** A 60s
  reconciler tick clears stale rows whose container is gone. Tail
  logs at `LOG_LEVEL=debug` for the `reconciler: tick complete`
  heartbeat to confirm it's running.

### Known limitations

- **Concurrent workflows that publish host ports collide.** The
  default `myoung34/github-runner` image uses Docker-out-of-Docker
  (the runner shares the host's `/var/run/docker.sock`), so any
  `docker run -p 5432:5432` your workflow performs binds the host's
  port — two parallel jobs both trying to start Postgres on `:5432`
  will conflict. The same applies to anything that contends for a
  host-global resource (named volumes, bridge networks of a fixed
  name, `--network=host` services, etc.). Workarounds:
  - Use ephemeral ports in your workflow (`-p 0:5432` and read the
    assigned port from `docker port`), or rely on container-to-
    container networking instead of `-p`.
  - Switch to a Docker-in-Docker runner image so each runner has
    its own Docker daemon. [Sysbox](https://github.com/nestybox/sysbox)
    is the cleanest path here — provide a custom `RUNNER_COMMAND`
    that adds `--runtime=sysbox-runc` and drops the
    `/var/run/docker.sock` mount. gharp doesn't ship a Sysbox-based
    template by default because it requires installing the Sysbox
    runtime on the host.

### Ops APIs

- `GET /` — serves the built-in dashboard for status, recent jobs, filters, and retry/cancel controls.
- `GET /healthz` — returns `ok`.
- `GET /jobs` — returns recent jobs as JSON.
- `GET /jobs/{job_id}` — returns full job detail, including stored webhook payload.
- `POST /jobs/{job_id}/retry` — retries a completed job locally (status resets to pending and is enqueued).
- `POST /jobs/{job_id}/cancel` — cancels a pending/dispatched job locally.
- `GET /stats` — returns dashboard-friendly JSON counts for jobs, runners, and capacity.
- `GET /metrics` — returns Prometheus text-format gauges for current job and runner counts.

The dashboard is a self-contained HTML/CSS/JS page served by gharp. It
loads `/stats` and `/jobs` from the browser, stores an entered
`ADMIN_TOKEN` in `sessionStorage`, and uses the same retry/cancel APIs
as curl clients.

`/jobs` supports query params:

| Param | Description |
| --- | --- |
| `status` | One or more of `pending`, `dispatched`, `in_progress`, `completed`. Repeated (`?status=pending&status=dispatched`) or CSV (`?status=pending,dispatched`). |
| `repo` | Exact `owner/name` filter. |
| `limit` | Default `100`, max `500`. |

`/jobs` rows include metadata captured from `workflow_job` payloads:
`job_name`, `run_id`, `run_attempt`, `workflow_name`.

**Authentication:** if `ADMIN_TOKEN` is unset or empty the endpoints are
open. If set, every request must include `Authorization: Bearer <token>`.

```bash
# Open mode
curl "http://localhost:8080/jobs?status=pending&limit=50"

# Token-protected mode
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/jobs?status=pending,dispatched&repo=owner/repo"

# Job detail
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/jobs/123456789"

# Local control actions
curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/jobs/123456789/retry"

curl -X POST -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/jobs/123456789/cancel"

# Dashboard stats JSON
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/stats"

# Prometheus metrics
curl -H "Authorization: Bearer $ADMIN_TOKEN" \
  "http://localhost:8080/metrics"
```

`/stats` exposes JSON shaped for the built-in dashboard:
`jobs` and `runners` maps keyed by status, plus `capacity` with
`max_concurrent_runners`, `active_runners`, and `available_slots`.

`/metrics` exposes low-cardinality current-state gauges:
`gharp_jobs_total{status}`, `gharp_runners_total{status}`,
and `gharp_max_concurrent_runners`.
Use `gharp_jobs_total{status="pending"}` as the canonical pending-jobs signal,
and `sum(gharp_runners_total{status=~"starting|idle|busy"})` for active runners.
Keep `/metrics` for Prometheus; use `/stats` for UI and JSON clients.

### Logs

`gharp` writes structured (`slog`) lines to stderr. `LOG_LEVEL=debug`
adds per-event detail (webhook payloads, dispatch decisions, store
writes). Pipe to your usual log shipper via Docker's logging driver.
