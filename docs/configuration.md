# Configuration reference

All configuration is read from environment variables at startup
(`internal/config/config.go`). Empty-string and unset are treated the
same.

## Required

| Variable | Description |
| --- | --- |
| `BASE_URL` | Public HTTPS URL where GitHub can reach gharp (e.g. `https://gharp.example.com`). Trailing slash is stripped. **Sticky** — baked into the GitHub App's webhook + OAuth-callback URLs at `/setup` time; changing it later only triggers a `BASE_URL drift` warning. To migrate, re-run `/setup` (creating a fresh App) or revert. |

## Server

| Variable | Default | Description |
| --- | --- | --- |
| `PORT` | `8080` | HTTP listen port. |
| `STORE_DSN` | `file:/data/gharp.db?_pragma=journal_mode(WAL)` (in the published `muhac/gharp` image) ・ `file:gharp.db?_pragma=journal_mode(WAL)` (when running the binary directly) | SQLite DSN. The image sets a default that lands in `/data`, which is declared as a `VOLUME` — mount a host directory or named volume there to survive container restarts. Override if you want the DB elsewhere. |
| `LOG_LEVEL` | `info` | One of `debug` / `info` / `warn` (alias `warning`) / `error`. Unknown values fall back to `info`. |

## Runners

| Variable | Default | Description |
| --- | --- | --- |
| `RUNNER_IMAGE` | `myoung34/github-runner:latest` | Image used as `{{.Image}}` in the dispatch command. Pin a tag or digest for reproducibility. |
| `RUNNER_NAME_PREFIX` | `gharp-` | Prefix for runner/container names AND the namespace the reconciler's orphan sweep operates in. Override only when running multiple gharp deployments against the same docker daemon (or when running integration tests on a host that already has unrelated `gharp-` containers); each deployment must use a distinct prefix or its reconciler will reach into the others' containers. Empty string fails startup. |
| `RUNNER_LABELS` | `self-hosted` | Comma-separated list of labels this pool can satisfy. A `workflow_job` is accepted only if **every** one of its `runs-on` labels appears here (matching GitHub's cumulative `runs-on` semantics — a runner must have all required labels to be eligible). `self-hosted` is implicit (GitHub auto-assigns it to every self-hosted runner) and is always treated as satisfiable, even if you don't list it. Comparison is case-insensitive. To partition multiple gharp deployments, use a unique non-`self-hosted` label per pool. |
| `ALLOW_PUBLIC_REPOS` | `false` | Public-repo safety guard. By default, queued `workflow_job` webhooks whose payload has `repository.private=false` are dropped with a warning before any job is queued. Set to `true` only if you intentionally want this pool to serve all public repos where the App is installed. |
| `REPO_ALLOWLIST` | _(unset)_ | Comma-separated list of exact public repo full names (`owner/repo`) allowed even when `ALLOW_PUBLIC_REPOS=false`. Matching is case-insensitive. This is a public-repo bypass list only; private repo access is controlled by the GitHub App installation scope. |
| `MAX_CONCURRENT_RUNNERS` | `4` | Global cap on simultaneously running ephemeral runners. Must be `>= 1` (`0` or negative fails startup). Non-integer strings silently fall back to the default. |
| `RUNNER_MAX_LIFETIME` | `2h` | Hard upper bound on how long a runner row can stay in the active set before the reconciler force-removes its container and marks the row finished. Defends against EPHEMERAL runners that registered but never claimed a job (no `in_progress` webhook ever arrives, the cap slot would be held forever). Parsed via Go's `time.ParseDuration` (`90m`, `2h30m`, `45s`). Must be a positive duration; `0` or negative fails startup. Unparseable strings silently fall back to the default. **This is also the maximum wall-clock time a single job can run** — a runner mid-job at the threshold gets `docker rm -f`ed and the job will fail. If you have long-running jobs (builds, training, large test suites), raise this above your slowest expected job duration. |
| `DOCKER_HOST` | _(unset)_ | Docker daemon endpoint. Unset = use the default socket the Docker SDK picks (`/var/run/docker.sock` on Linux). Override for remote daemons (e.g. `tcp://docker:2375`). |
| `RUNNER_COMMAND` | _(see below)_ | JSON array of argv (no shell). Required placeholders are validated at startup: `{{.ContainerName}}`, `{{.RegistrationToken}}`, `{{.RunnerName}}`, `{{.RepoURL}}`, `{{.Labels}}`. Optional: `{{.Image}}`. Empty array, non-array JSON, or a missing required placeholder cause startup to fail. |

Default `RUNNER_COMMAND`:

```text
docker run --rm \
  --name {{.ContainerName}} \
  -e REPO_URL={{.RepoURL}} \
  -e RUNNER_TOKEN={{.RegistrationToken}} \
  -e RUNNER_NAME={{.RunnerName}} \
  -e LABELS={{.Labels}} \
  -e EPHEMERAL=1 \
  {{.Image}}
```

Override to add `--network`, mount the Docker socket into the runner,
attach a workdir volume, set log limits, etc. See `docker-compose.yml`
for a worked example.

## GitHub Enterprise Server

| Variable | Default | Description |
| --- | --- | --- |
| `GITHUB_API_BASE` | `https://api.github.com` | API base. For GHES: `https://gh.example.com/api/v3`. Trailing slash is stripped; must be an absolute URL. |
| `GITHUB_WEB_BASE` | `https://github.com` | Web base used to build runner repository URLs (`{{.RepoURL}}`). For GHES, set to your enterprise host. |

## Validation behavior

- `BASE_URL` missing → startup fails.
- `GITHUB_API_BASE` / `GITHUB_WEB_BASE` not absolute (no scheme/host) → startup fails.
- `RUNNER_COMMAND` malformed JSON, empty array, or missing required placeholder → startup fails.
- `ALLOW_PUBLIC_REPOS` only enables on literal `true` (case-insensitive); unset, empty, or any other value is `false`.
- `REPO_ALLOWLIST` empty or unset means no public repo bypasses.
- `MAX_CONCURRENT_RUNNERS` non-integer → silently uses default. `0` or negative → startup fails (otherwise the cap-exceeded branch would re-enqueue every job forever).
- `RUNNER_MAX_LIFETIME` unparseable → silently uses default. `0` or negative → startup fails.
- `LOG_LEVEL` unknown → silently uses `info`.
- `BASE_URL` differs from value persisted in `app_config` → warning logged, startup continues.
