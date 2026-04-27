# Components

> Implementation status and dependency map for `gharp`.

## Status legend

| Symbol | Meaning |
|---|---|
| ✅ | Fully implemented + tested |
| 🔧 | Partial / skeleton (struct/interface defined, logic missing) |
| ❌ | Stub (returns `errNotImplemented` or 501) |

---

## Component map

```text
cmd/gharp/main.go   ✅  wiring only; no business logic
│
├── internal/config  ✅  env loading, template validation
│
├── internal/store   ❌  interface defined; sqlite impl is a stub
│     models.go  ✅  AppConfig / Installation / Job / Runner structs
│     store.go   ✅  Store interface
│     sqlite.go  ❌  all methods return errNotImplemented
│
├── internal/github  (mixed)
│     client.go      ✅  NewClient, http.Client wrapper
│     manifest.go    🔧  BuildManifest ✅ | ConvertCode ❌
│     auth.go        ❌  AppJWT / InstallationToken stubs
│     runners.go     ❌  RegistrationToken / List / Delete stubs
│
├── internal/runner  ✅  template rendering, exec.Command launch, tests
│
├── internal/scheduler  (mixed)
│     types.go       ✅  WorkflowJob payload structs
│     scheduler.go   🔧  New / Enqueue / Run defined; dispatch is a TODO
│
└── internal/httpapi  (mixed)
      router.go      ✅  ServeMux wiring
      handlers/
        health.go    ✅  GET /healthz → 200 "ok"
        setup.go     ❌  GET /setup → 501
        callback.go  ❌  GET /github/app/callback → 501
        webhook.go   ❌  POST /github/webhook → 501
```

---

## Per-component detail

### `internal/config` ✅

Env-var loading + validation. One `Config` struct, one `Load()` function.

- Validates 5 required `{{.Placeholder}}` literals in the runner command template at startup.
- Parses `LOG_LEVEL` into `slog.Level`.
- Covered by `config_test.go`.

**No changes needed for v1.**

---

### `internal/store` ❌ → needs full implementation

**Schema** (4 tables):

| Table | Key columns | Notes |
|---|---|---|
| `app_config` | `app_id`, `webhook_secret`, `pem`, `base_url`, `slug` | Single row; upsert on startup after manifest conversion |
| `installations` | `installation_id`, `account_login`, `account_type` | One row per GitHub App installation |
| `jobs` | `job_id` (PK), `repo_full_name`, `action`, `labels`, `status`, `received_at` | `INSERT OR IGNORE` for dedup; status: `pending → in_progress → completed` |
| `runners` | `container_name` (PK), `runner_name`, `repo_full_name`, `labels`, `status`, `started_at`, `finished_at` | status: `starting → idle → busy → finished` |

**Methods to implement** (from `Store` interface):

```go
// app_config
SaveAppConfig(ctx, *AppConfig) error
GetAppConfig(ctx) (*AppConfig, error)

// installations
SaveInstallation(ctx, *Installation) error
GetInstallationByRepo(ctx, repoFullName string) (*Installation, error)
ListInstallations(ctx) ([]*Installation, error)

// jobs
InsertJob(ctx, *Job) error          // INSERT OR IGNORE — dedup guard
GetJob(ctx, jobID int64) (*Job, error)
UpdateJobStatus(ctx, jobID int64, status string, extra ...any) error
ListPendingJobs(ctx) ([]*Job, error) // for crash-recovery re-enqueue

// runners
InsertRunner(ctx, *Runner) error
UpdateRunnerStatus(ctx, containerName, status string, extra ...any) error
CountActiveRunners(ctx) (int, error) // concurrency cap check
```

**Implementation notes:**
- Use `modernc.org/sqlite` (pure Go, no CGO).
- Run schema migrations with embedded SQL at `OpenSQLite` time.
- All queries use `context.Context` for cancellation.

---

### `internal/github/auth` ❌

**`AppJWT()`**
- Read `app_id` + `pem` from `store.GetAppConfig`.
- Build JWT: `iat = now-60s`, `exp = now+10m`, `iss = app_id`, signed RS256.
- Return the signed token string.

**`InstallationToken(ctx, installationID)`**
- Call `POST /app/installations/{id}/access_tokens` with `Authorization: Bearer <AppJWT>`.
- Cache result in-memory keyed by `installation_id`, evict at `exp - 5min`.
- Return `token` string.

---

### `internal/github/manifest` 🔧

`BuildManifest(baseURL)` — done.

**`ConvertCode(ctx, code)`** ❌
- `POST https://api.github.com/app-manifests/<code>/conversions`
- No auth header (single-use code is the credential).
- Parse response into `AppCredentials{AppID, WebhookSecret, PEM, Slug, ClientID}`.
- Return immediately — code is short-lived and single-use.

---

### `internal/github/runners` ❌

**`RegistrationToken(ctx, owner, repo, installToken)`**
- `POST /repos/{owner}/{repo}/actions/runners/registration-token`
- Auth: `Bearer <installToken>`.
- Return `token` string. Do **not** cache — single-use under `EPHEMERAL=1`.

**`ListRepoRunners(ctx, owner, repo, installToken)`**
- `GET /repos/{owner}/{repo}/actions/runners`
- Needed by reconciliation loop (v1.1).

**`DeleteRepoRunner(ctx, owner, repo, runnerID int64, installToken)`**
- `DELETE /repos/{owner}/{repo}/actions/runners/{runner_id}`
- Needed by reconciliation loop (v1.1).

---

### `internal/httpapi/handlers/setup.go` ❌

**`GET /setup`**
1. Check `store.GetAppConfig` — if found and `base_url` matches, redirect to installed state.
2. Generate random `state` value (16 bytes hex); set as `HttpOnly` cookie `gharp_state`.
3. Build manifest JSON via `github.BuildManifest(cfg.BaseURL)`.
4. Render `setup.html` — form POST target is `https://github.com/settings/apps/new?state=<state>`, manifest embedded as hidden field.

---

### `internal/httpapi/handlers/callback.go` ❌

**`GET /github/app/callback?code=<code>&state=<state>`**
1. Read `gharp_state` cookie; constant-time compare with `state` query param. Return 400 on mismatch (CSRF guard).
2. Call `github.ConvertCode(ctx, code)` — must be done immediately (code expires ~10 min, single-use).
3. Call `store.SaveAppConfig` with returned credentials.
4. Clear the `gharp_state` cookie.
5. Render `setup_done.html` with install link `https://github.com/apps/<slug>/installations/new`.

---

### `internal/httpapi/handlers/webhook.go` ❌

**`POST /github/webhook`** (must return < 10s — all slow work deferred to scheduler)

1. Read raw body into `[]byte` (don't use `r.Body` directly — must verify HMAC on raw bytes).
2. Verify `X-Hub-Signature-256`: `HMAC-SHA256(secret, rawBody)` in constant-time compare.
3. Switch on `X-GitHub-Event` header:
   - `installation` / `installation_repositories` → upsert installation row.
   - `workflow_job` → parse payload, dispatch to scheduler.
   - anything else → 200 (ignore).
4. For `workflow_job`:
   - Filter: drop if `action != "queued"` and not `in_progress` or `completed`.
   - For `queued`: call `store.InsertJob` (INSERT OR IGNORE) then `scheduler.Enqueue(jobID)`.
   - For `in_progress`: `store.UpdateJobStatus(jobID, "in_progress", runnerName, runnerID)`.
   - For `completed`: `store.UpdateJobStatus(jobID, "completed", conclusion)`.

---

### `internal/scheduler/scheduler.go` 🔧

Struct and `New` / `Enqueue` defined. `Run` loop has a TODO.

**`Run(ctx)` — worker loop to implement:**

```text
on startup:
  pending := store.ListPendingJobs()
  for each: channel <- job.ID   (crash recovery)

loop:
  jobID := <-channel
  active := store.CountActiveRunners()
  if active >= cfg.MaxConcurrentRunners:
    sleep briefly, re-enqueue, continue

  job := store.GetJob(jobID)
  installation := store.GetInstallationByRepo(job.RepoFullName)
  installToken := github.InstallationToken(installation.ID)
  regToken := github.RegistrationToken(owner, repo, installToken)

  runner := &store.Runner{
    ContainerName: generateName(),
    RunnerName:    generateName(),
    RepoFullName:  job.RepoFullName,
    Labels:        job.Labels,
    Status:        "starting",
    StartedAt:     time.Now(),
  }
  store.InsertRunner(runner)

  spec := runner.Spec{
    ContainerName:     runner.ContainerName,
    RegistrationToken: regToken,
    RunnerName:        runner.RunnerName,
    RepoURL:           "https://github.com/" + job.RepoFullName,
    Labels:            runner.Labels,
    Image:             cfg.RunnerImage,
  }
  rn.Launch(ctx, spec)   // non-blocking — cmd.Start() only
```

---

## Dependency graph (implementation order)

```text
1. store/sqlite.go          ← foundation; nothing works without it
2. github/auth.go           ← needs store.GetAppConfig
3. github/manifest.go       ← ConvertCode; needs only http.Client
4. github/runners.go        ← needs auth tokens
5. handlers/setup.go        ← needs store + BuildManifest
6. handlers/callback.go     ← needs store + ConvertCode
7. handlers/webhook.go      ← needs store + scheduler.Enqueue
8. scheduler/scheduler.go   ← needs store + auth + runners + runner.Launch
```

Items 2 and 3 can be done in parallel (no shared dependency).
Items 5, 6, 7 can be developed in parallel once store and github/* are done.

---

## Critical invariants (from architecture)

- Verify HMAC on **raw body**, before any JSON parse.
- Webhook handler MUST return < 10s. Steps 1-5 only; everything slow is in the worker.
- Dedupe by `workflow_job.id` — `INSERT OR IGNORE` is the canonical guard; do not rely on channel dedup.
- Channel is best-effort; sqlite is durable. On restart, scan `pending` jobs and re-enqueue.
- Registration tokens are single-use — never cache.
- Installation tokens are cacheable for ~55 min (1h TTL − 5min margin).
- Concurrency cap check happens **before** minting any token.
- Filter on `runs-on` labels early (step 3 of webhook) — don't insert jobs we'll never serve.
