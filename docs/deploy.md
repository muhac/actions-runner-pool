# Deployment guide

This walks through a real deployment of gharp on a single host: Docker
Compose, a public HTTPS URL via Cloudflare Tunnel (or ngrok for dev),
and the GitHub App setup.

For the full env-variable reference, see [configuration.md](configuration.md).

## Requirements

- A host with Docker (any modern Linux; tested on amd64 + arm64).
- Access to the host's Docker daemon socket (`/var/run/docker.sock`).
- A public HTTPS URL that GitHub can reach.
- A GitHub account or organization where you'll install the App.

> ⚠️ **Untrusted code**: gharp runs CI jobs from any repo where the App
> is installed, in containers that share the host Docker socket. Run
> only on a dedicated VM, cloud instance, or homelab node — not on a
> machine that holds secrets you wouldn't paste into a workflow.

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

GitHub needs to POST webhooks to `${BASE_URL}/github/webhook`. Pick one:

### Cloudflare Tunnel (recommended for production)

```bash
cloudflared tunnel create gharp
cloudflared tunnel route dns gharp gharp.example.com
# In ~/.cloudflared/config.yml, route gharp.example.com → http://localhost:8080
cloudflared tunnel run gharp
```

Then `BASE_URL=https://gharp.example.com`.

### ngrok (fastest for dev)

```bash
ngrok http 8080
# Note the https://<random>.ngrok-free.app URL.
```

Set `BASE_URL` to that URL. Note that the ngrok URL changes each restart
on the free tier — if it changes, you'll need to re-run `/setup` (see
"BASE_URL drift" in `configuration.md`).

### Tailscale Funnel

Works the same way; expose port 8080 and use the Funnel hostname.

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

Open `${BASE_URL}/setup` and click **Create GitHub App**.

This drives the GitHub App Manifest flow:

1. Browser POSTs the manifest to GitHub.
2. GitHub creates the App, issues a temporary code, and redirects to
   `${BASE_URL}/github/app/callback?code=...&state=...`.
3. gharp exchanges the code for the App's `private_key`,
   `webhook_secret`, and `client_secret`, persists them in `app_config`,
   and renders the **install link**.
4. Click the install link, choose the repos (or "All repositories"), and
   submit.

You're done. From this point, every `workflow_job` whose `runs-on` set
intersects `RUNNER_LABELS` will get a fresh runner.

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

The SQLite DB on the `gharp-data` volume is preserved. Schema migrations
are idempotent and run on startup.

### Backup

The only stateful piece is `gharp-data` (SQLite). Snapshot the volume,
or `sqlite3 /data/gharp.db ".backup '/backup/gharp.db'"` from a sidecar.

### Rotating credentials

`BASE_URL`, the App's `private_key`, and the `webhook_secret` are baked
into the App at `/setup` time. There's no rotation flow in v1 — to
change any of them, delete the App on GitHub, drop the `app_config` row
(or wipe the DB), and re-run `/setup`. See `docs/architecture.md`
§ "Configuration immutability and key rotation" for the design rationale.

### Troubleshooting

- **No webhooks landing.** Confirm `BASE_URL` is reachable from the
  public internet (`curl ${BASE_URL}/setup` from outside your network).
  Check the App's "Recent Deliveries" page on GitHub for HTTP errors.
- **Webhook 401.** The signing secret in `app_config` doesn't match the
  one GitHub holds. This usually means you swapped the DB but kept the
  old App, or vice versa. Re-run `/setup`.
- **Runners start but never pick up the job.** Check `RUNNER_LABELS` — a
  job is accepted only if some label in `runs-on` is in the allowlist.
- **`BASE_URL drift` warning at startup.** Your `BASE_URL` env differs
  from the URL stored in `app_config`. Either revert the env or re-run
  `/setup`. See `configuration.md`.
- **Runners pile up after jobs finish.** Should not happen with the
  default `--rm` command. If you removed `--rm` in a custom
  `RUNNER_COMMAND`, restore it or add your own GC.

### Logs

`gharp` writes structured (`slog`) lines to stderr. `LOG_LEVEL=debug`
adds per-event detail (webhook payloads, dispatch decisions, store
writes). Pipe to your usual log shipper via Docker's logging driver.
