# Components

> Detailed, phased design for `gharp` v1. Companion to `architecture.md` — the
> architecture doc says *what* and *why*; this doc says *in what order* and
> *with which signatures*.

## Status snapshot (2026-04-27)

| Package | File | State | Notes |
|---|---|---|---|
| `cmd/gharp` | `main.go` | ✅ | Wiring only. No business logic. |
| `internal/config` | `config.go` (+test) | ✅ | Env loading, template placeholder validation. |
| `internal/runner` | `docker.go` (+test) | ✅ | Template render → `exec.Command(...).Start()`. |
| `internal/store` | `models.go`, `store.go` | 🔧 | Interface + structs exist; needs new methods (see Phase 0). |
| `internal/store` | `sqlite.go` | ❌ | All methods return `errNotImplemented`. |
| `internal/github` | `client.go` | ✅ | `http.Client` wrapper. |
| `internal/github` | `manifest.go` | 🔧 | `BuildManifest` ✅, `ConvertCode` ❌. |
| `internal/github` | `auth.go` | ❌ | `AppJWT`, `InstallationToken` stubs. |
| `internal/github` | `runners.go` | ❌ | `RegistrationToken` stub; List/Delete deferred to v1.1. |
| `internal/scheduler` | `types.go` | ✅ | `WorkflowJobEvent` payload. |
| `internal/scheduler` | `scheduler.go` | 🔧 | `New` / `Enqueue` ✅, `Run` is a TODO loop. |
| `internal/httpapi` | `router.go` | ✅ | `ServeMux` wiring. |
| `internal/httpapi/handlers` | `health.go` | ✅ | `GET /healthz` → 200. |
| `internal/httpapi/handlers` | `setup.go` | ❌ | 501. |
| `internal/httpapi/handlers` | `callback.go` | ❌ | 501. |
| `internal/httpapi/handlers` | `webhook.go` | ❌ | 501. |

Legend: ✅ done + tested · 🔧 partial · ❌ stub.

---

## Implementation phases

Each phase has: **goal**, **deliverables**, **acceptance criteria**, and lands
as one or more commits on the `p-39` branch (per CLAUDE.md rule 2). Phases run
in order; items inside a phase marked **‖ parallelizable** can be split.

### Phase 0 — Interface alignment (prep, no behavior change)

Goal: lock the `Store` interface and `Job` model so Phase 1 has a stable target
and Phase 3 (handlers) and Phase 4 (scheduler) don't have to chase rename
churn.

Changes:

1. `internal/store/models.go` — add `Conclusion string` to `Job` (carries the
   value from `workflow_job: completed` payloads).
2. `internal/store/store.go` — add the methods the worker and webhook need but
   the current interface doesn't expose:

   ```go
   // Need to load the full job row inside the worker (repo, labels).
   GetJob(ctx context.Context, jobID int64) (*Job, error)

   // Distinguish in_progress (binds runner) from completed (records conclusion).
   MarkJobInProgress(ctx context.Context, jobID int64, runnerID int64, runnerName string) error
   MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) error
   ```

   Remove the old polymorphic `UpdateJobStatus(...)` — splitting it kills the
   `extra ...any` smell and matches the two real call sites (webhook on
   `in_progress`, webhook on `completed`).

3. Confirm spelling for the rest of the interface (these are the **canonical**
   names; `architecture.md` and any prior drafts that disagree are wrong):

   | Method | Purpose |
   |---|---|
   | `SaveAppConfig` / `GetAppConfig` | App credentials (singleton row) |
   | `UpsertInstallation` / `ListInstallations` / `InstallationForRepo` | Per-installation rows |
   | `InsertJobIfNew(j) (inserted bool, err error)` | Dedupe guard via `INSERT OR IGNORE` |
   | `GetJob`, `MarkJobInProgress`, `MarkJobCompleted`, `PendingJobs` | Job state machine |
   | `InsertRunner` / `UpdateRunnerStatus` / `ActiveRunnerCount` / `ListActiveRunners` | Runner lifecycle |

Acceptance:
- `go build ./...` green (sqlite impl still stubbed but matches new interface).
- No test changes yet — Phase 1 brings them.

Commit: `refactor(store): split UpdateJobStatus, add GetJob, Conclusion field`.

---

### Phase 1 — `internal/store/sqlite.go` (foundation)

Goal: real persistence. Everything downstream depends on it.

Driver: `modernc.org/sqlite` (pure Go, no CGO). Add to `go.mod`.

#### 1.1 Schema (embedded SQL, applied at `OpenSQLite` time)

```sql
CREATE TABLE IF NOT EXISTS app_config (
  id              INTEGER PRIMARY KEY CHECK (id = 1),  -- singleton
  app_id          INTEGER NOT NULL,
  slug            TEXT    NOT NULL,
  webhook_secret  TEXT    NOT NULL,
  pem             BLOB    NOT NULL,
  client_id       TEXT    NOT NULL,
  client_secret   TEXT    NOT NULL DEFAULT '',
  base_url        TEXT    NOT NULL,
  created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS installations (
  id             INTEGER PRIMARY KEY,           -- GitHub installation_id
  account_id     INTEGER NOT NULL,
  account_login  TEXT    NOT NULL,
  account_type   TEXT    NOT NULL,              -- "User" | "Organization"
  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS jobs (
  id           INTEGER PRIMARY KEY,             -- workflow_job.id
  repo         TEXT    NOT NULL,                -- "owner/repo"
  action       TEXT    NOT NULL,                -- "queued" | "in_progress" | "completed"
  labels       TEXT    NOT NULL,                -- comma-joined runs-on labels
  dedupe_key   TEXT    NOT NULL UNIQUE,         -- usually str(workflow_job.id)
  status       TEXT    NOT NULL,                -- "pending" | "in_progress" | "completed"
  conclusion   TEXT    NOT NULL DEFAULT '',
  runner_id    INTEGER NOT NULL DEFAULT 0,
  runner_name  TEXT    NOT NULL DEFAULT '',
  received_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

CREATE TABLE IF NOT EXISTS runners (
  container_name TEXT    PRIMARY KEY,
  repo           TEXT    NOT NULL,
  runner_name    TEXT    NOT NULL,              -- v1.1 reconciliation joins on this
  labels         TEXT    NOT NULL,
  status         TEXT    NOT NULL,              -- "starting" | "idle" | "busy" | "finished"
  started_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  finished_at    DATETIME
);
CREATE INDEX IF NOT EXISTS idx_runners_status ON runners(status);
```

Why both `container_name` and `runner_name` on day one: the v1.1 reconciliation
loop joins our row to `docker inspect` (by container name) and to the GitHub
`/runners` API response (by runner name). Storing both now is cheap; backfilling
later is impossible (architecture.md §"Ghost runners").

#### 1.2 Method implementations

- All queries take `context.Context` and use `db.QueryContext` / `ExecContext`.
- `InsertJobIfNew`: `INSERT … ON CONFLICT(dedupe_key) DO NOTHING`, then check
  `RowsAffected` for the `inserted bool`.
- `ActiveRunnerCount`: `SELECT count(*) FROM runners WHERE status IN ('starting','idle','busy')`.
- `MarkJobInProgress` / `MarkJobCompleted` also bump `updated_at = CURRENT_TIMESTAMP`.
- `Close()` closes the underlying `*sql.DB`.

#### 1.3 Tests (`sqlite_test.go`)

In-memory DSN per test, exercise:
- Schema applies cleanly (open then re-open is idempotent).
- `InsertJobIfNew` returns `true` first time, `false` second time, no error.
- `MarkJobInProgress` then `MarkJobCompleted` round-trips through `GetJob`.
- `ActiveRunnerCount` counts only the three live statuses.
- `PendingJobs` returns rows in `received_at` order.

Acceptance:
- `go test ./internal/store/...` green.
- `cmd/gharp` boots against a real file DSN without panicking (`docker compose up` smoke).

Commits (suggested split):
- `feat(store): sqlite schema + migrations`
- `feat(store): implement Store methods`
- `test(store): sqlite round-trips`

---

### Phase 2 — `internal/github/*` (API client) ‖ parallelizable across files

Goal: every GitHub call the rest of the system needs. No retry/backoff yet —
add in v1.1.

#### 2.1 `auth.go`

```go
// AppJWT mints a short-lived JWT (RS256) signed with the App private key.
// iat = now-60s (clock skew margin), exp = now+10m, iss = app_id.
func (c *Client) AppJWT(ctx context.Context, st store.Store) (string, error)

// InstallationToken returns a cached or freshly minted installation token.
// Cache key: installation_id. TTL: token.expires_at - 5min margin.
func (c *Client) InstallationToken(ctx context.Context, st store.Store, installationID int64) (string, error)
```

- JWT signer: `github.com/golang-jwt/jwt/v5` (small, well-maintained).
- Installation-token cache: `sync.Map` keyed by `int64` → `struct{token string; exp time.Time}`. Read under lock, refresh under lock.
- Endpoint: `POST {api}/app/installations/{id}/access_tokens` with header `Authorization: Bearer <appJWT>`.

#### 2.2 `manifest.go::ConvertCode`

```go
type AppCredentials struct {
    AppID         int64
    Slug          string
    WebhookSecret string
    PEM           []byte
    ClientID      string
    ClientSecret  string
}

func (c *Client) ConvertCode(ctx context.Context, code string) (*AppCredentials, error)
```

- `POST https://api.github.com/app-manifests/<code>/conversions`. No auth header — the single-use code *is* the credential.
- Decode JSON; map `pem` (string) → `[]byte`.
- No retry: code expires in ~10 min and is single-use; any failure surfaces to the user, who restarts the manifest flow.

#### 2.3 `runners.go::RegistrationToken`

```go
func (c *Client) RegistrationToken(ctx context.Context, owner, repo, installToken string) (string, error)
```

- `POST {api}/repos/{owner}/{repo}/actions/runners/registration-token`, `Authorization: Bearer <installToken>`.
- Return `token` only — `expires_at` is irrelevant under `EPHEMERAL=1` (single use).
- **Never cache** (architecture invariant).

`ListRepoRunners` and `DeleteRepoRunner` deferred to v1.1 (reconciliation).

#### 2.4 Tests (lightweight)

- `auth_test.go`: assert JWT header/claims (decode without verifying — we don't need to validate our own signature, just shape).
- `manifest_test.go`: `httptest.NewServer` returning canned conversion JSON; assert struct fields.
- `runners_test.go`: same, for registration-token shape.

Acceptance:
- `go test ./internal/github/...` green.

Commits: `feat(github): app JWT + installation token cache`, `feat(github): manifest ConvertCode`, `feat(github): registration token`.

---

### Phase 3 — `internal/httpapi/handlers/*` ‖ parallelizable across handlers

Depends on: Phase 1 (store) + Phase 2 (github client). The three handlers are
independent of each other.

#### 3.1 `setup.go` — `GET /setup`

1. `st.GetAppConfig(ctx)`: if found and `BaseURL` matches `cfg.BaseURL`,
   render `setup_done.html` with the install link
   `https://github.com/apps/<slug>/installations/new`.
2. Generate state: `crypto/rand` 16 bytes → hex.
3. Set cookie `gharp_state` (HttpOnly, Secure if BaseURL is https, SameSite=Lax, 10-min expiry).
4. Build manifest: `github.BuildManifest(cfg.BaseURL)` → JSON-encode → embed as
   hidden field in `setup.html`. Form `action` =
   `https://github.com/settings/apps/new?state=<state>`, method `POST`.

#### 3.2 `callback.go` — `GET /github/app/callback?code=…&state=…`

1. Read `gharp_state` cookie. `subtle.ConstantTimeCompare` against `state` query.
   Mismatch → 400 ("invalid state").
2. `gh.ConvertCode(ctx, code)` — no queueing; code is short-lived.
3. `st.SaveAppConfig(...)` with returned credentials + `cfg.BaseURL`.
4. Clear `gharp_state` cookie (set with `MaxAge=-1`).
5. Render `setup_done.html` with install link.

#### 3.3 `webhook.go` — `POST /github/webhook`

Hard ceiling: ≤ ~3s typical, < 10s worst. **No GitHub API calls, no `docker run`.**

Order of operations (cannot reorder):

1. **Read raw body once** into `[]byte` (the HMAC must be computed over the raw bytes — re-serializing breaks it).
2. **Verify HMAC** of `X-Hub-Signature-256`:
   `hmac.Equal([]byte(received), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))`.
   Use `st.GetAppConfig` for the secret. Mismatch → 401.
3. Switch on `X-GitHub-Event`:

   | Event | Action |
   |---|---|
   | `installation` (action `created` / `deleted`) | Upsert / no-op respectively. |
   | `installation_repositories` (`added` / `removed`) | v1: log only (workflow_job payloads carry repo). v1.1: maintain per-repo cache. |
   | `workflow_job` | See §3.3.1. |
   | anything else | 200, no-op. |

##### 3.3.1 `workflow_job` handling

- Parse into `scheduler.WorkflowJobEvent`.
- **Label filter (architecture invariant: filter early):** intersect
  `payload.workflow_job.labels` with `cfg.RunnerLabels` (new — see Phase 5
  config note). If empty intersection → 200, do not insert. Skip filter when
  `RunnerLabels` is unset (default = "serve everything").
- Branch on `action`:

  | action | Steps |
  |---|---|
  | `queued` | Build `Job` row → `inserted, err := st.InsertJobIfNew(j)`. If `inserted`, `sch.Enqueue(j.ID)`. |
  | `in_progress` | `st.MarkJobInProgress(jobID, runnerID, runnerName)` and `st.UpdateRunnerStatus(containerName, "busy")` (best-effort — match runner row by `runner_name`; v1 just logs if not found). |
  | `completed` | `st.MarkJobCompleted(jobID, conclusion)` and `st.UpdateRunnerStatus(containerName, "finished")`. |

- Always 200 unless HMAC fails (401) or body parse explodes (400). Never 500
  on transient store errors — log + 202 so GitHub doesn't retry-storm.

#### 3.4 Tests

- `webhook_test.go`: HMAC pass/fail, queued → InsertJobIfNew called once even
  on duplicate delivery, label filter rejects non-matching runs-on, action
  switch covers all three workflow_job actions.
- `callback_test.go`: state mismatch → 400; happy path calls `SaveAppConfig`
  with decoded credentials.
- `setup_test.go`: cookie set, manifest embedded, redirect URL points at github.com.

Acceptance:
- `go test ./internal/httpapi/...` green.

Commits, one per handler: `feat(handlers): setup`, `feat(handlers): callback`, `feat(handlers): webhook + label filter + dedupe`.

---

### Phase 4 — `internal/scheduler.Run` (close the loop)

Goal: make the hot path actually spawn containers.

#### 4.1 Startup replay

Before entering the select loop:

```go
pending, err := s.store.PendingJobs(ctx)
for _, j := range pending {
    s.Enqueue(j.ID)   // best-effort; channel is 256-buffered
}
```

If the channel can't hold them all, the rest stay `pending` in sqlite — a
later Enqueue or the next process restart picks them up. Don't block here.

#### 4.2 Worker loop

```go
for {
  select {
  case <-ctx.Done(): return ctx.Err()
  case jobID := <-s.jobCh:
      s.dispatch(ctx, jobID)   // errors logged, never propagated up
  }
}
```

`dispatch(ctx, jobID)`:

1. **Concurrency cap (before any API call).**
   `n, _ := s.store.ActiveRunnerCount(ctx)`; if `n >= cfg.MaxConcurrentRunners`,
   sleep `~2s` and `s.Enqueue(jobID)` (re-queue the same id), return.
2. `job := s.store.GetJob(ctx, jobID)` (skip if status != `pending`; another
   replay may have already advanced it).
3. `inst := s.store.InstallationForRepo(ctx, job.Repo)`. If nil → log, leave
   pending (architecture: user must install the App on this repo).
4. `installToken := s.gh.InstallationToken(ctx, s.store, inst.ID)`.
5. Parse `owner, repo := split(job.Repo)`; `regToken := s.gh.RegistrationToken(...)`.
6. Build runner row:

   ```go
   r := &store.Runner{
     ContainerName: generateName(),   // "gharp-<8hex>"
     RunnerName:    generateName(),   // separate id; both stored
     Repo:          job.Repo,
     Labels:        job.Labels,
     Status:        "starting",
     StartedAt:     time.Now(),
   }
   _ = s.store.InsertRunner(ctx, r)
   ```

7. `s.runner.Launch(ctx, runner.Spec{...})` (uses fields above + `cfg.RunnerImage`).
   On `Launch` error: `s.store.UpdateRunnerStatus(ctx, r.ContainerName, "finished")`
   and log. The job stays in whatever state webhook set; v1 does not retry —
   architecture explicitly defers retry/backoff to v1.1.

#### 4.3 Tests

- `scheduler_test.go` with fakes (in-memory store, mock github client, no-op
  launcher): startup replay enqueues pending jobs; concurrency cap re-queues;
  happy path inserts runner row in `starting` status.

Acceptance:
- `go test ./internal/scheduler/...` green.
- End-to-end smoke (manual): `curl` a fake `workflow_job: queued` payload at
  `/github/webhook` (with valid HMAC) and observe a `runners` row created.

Commit: `feat(scheduler): startup replay + dispatch loop`.

---

### Phase 5 — Startup validation, config additions, polish

Small but important pieces that don't fit a single component:

1. **`config.RunnerLabels []string`** — new env var `RUNNER_LABELS`
   (comma-separated). Empty = serve all. Drives the webhook label filter.
2. **`config.GitHubAPIBase string`** — defaults to `https://api.github.com`,
   override for GHES. Used by `internal/github/*`.
3. **BaseURL drift warning at boot**: in `cmd/gharp/main.go` after `OpenSQLite`,
   if `existing, _ := st.GetAppConfig(ctx); existing != nil && existing.BaseURL != cfg.BaseURL`,
   `log.Warn("BASE_URL changed since App was created; webhooks may be unreachable", ...)`. Don't block startup.
4. **`/setup` redirect on healthy install**: when both `app_config` and at
   least one `installations` row exist, `/setup` should still render the
   install link (in case the user wants to add another installation).
5. **README / `.env.example`** updated with new env vars.
6. **Manual end-to-end** with ngrok + a throwaway repo: setup flow → install →
   push a workflow_job → container runs → job succeeds → row marked completed.

Commits: `feat(config): RUNNER_LABELS, GitHubAPIBase`, `feat(main): BASE_URL drift warning`, `docs: env vars + quickstart`.

---

## Dependency graph (one-page)

```text
  Phase 0 (interface tweak)
        │
        ▼
  Phase 1 (store/sqlite)  ◄─ foundation
        │
        ├──► Phase 2.1 auth   ────┐
        ├──► Phase 2.2 manifest   │   (parallel)
        └──► Phase 2.3 runners ───┤
                                   │
                                   ▼
                ┌──────────── Phase 3 handlers (3 files, parallel) ────────────┐
                │   3.1 setup       3.2 callback       3.3 webhook              │
                └──────────────────────────────────┬───────────────────────────┘
                                                   ▼
                                       Phase 4 scheduler.Run
                                                   │
                                                   ▼
                                     Phase 5 startup validation + polish
```

## Critical invariants (re-stated for the implementer)

These come from `architecture.md` §"Critical invariants" and bind every phase:

- HMAC over the **raw body**, before any JSON parse.
- Webhook handler returns < 10s — only steps 1-5, never a `docker run` or GitHub API call.
- Dedupe by `workflow_job.id` via `INSERT … ON CONFLICT DO NOTHING`. Channel dedup is not authoritative.
- sqlite is the durable record; channel is best-effort. Replay `PendingJobs` on startup.
- Registration tokens are single-use; never cache.
- Installation tokens cached in-memory for ~55 min (1h TTL − 5 min margin).
- Concurrency cap checked **before** minting any token.
- Filter on `runs-on` labels at the webhook, not in the worker — keeps the
  `jobs` table from filling with rows we'll never serve.
- Always store `container_name` AND `runner_name` on the `runners` row, even
  though v1 doesn't read them yet — v1.1 reconciliation needs both.
