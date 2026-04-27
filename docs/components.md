# Components

> Detailed, phased design for `gharp` v1. Companion to `architecture.md` — the
> architecture doc says *what* and *why*; this doc says *in what order*, *with
> which signatures*, and *how to verify each component before moving on*.

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

## Test conventions (apply to every Phase below)

- **Unit**: pure Go test, no network, no docker. Default for everything below
  unless noted.
- **Integration**: `httptest.NewServer` for GitHub API stand-in, in-memory
  sqlite for the store. Allowed in Phases 1–4.
- **Manual smoke**: run a real binary against ngrok / a real repo. Listed
  explicitly when used; never the only signal for a component.
- Every component's Test plan ends with a one-line **Done when** assertion
  that names what makes it merge-ready.

---

## Implementation phases

Each phase has: **goal**, **deliverables**, **Test plan per component**,
**acceptance criteria for the phase**, and lands as one or more commits on a
phase-specific branch (per CLAUDE.md rule 2 — one PR per phase).

---

### Phase 0 — Interface alignment (prep, no behavior change)

**Goal:** lock the `Store` interface and `Job` model so Phase 1 has a stable
target and Phases 3–4 don't have to chase rename churn.

#### 0.1 `internal/store/models.go`

Add `Conclusion string` to `Job` (carries the value from `workflow_job:
completed` payloads).

**Test plan:**
- `go vet ./...` clean.
- Field is read by `MarkJobCompleted` (Phase 1) and written by webhook (Phase 3).
- **Done when:** the type compiles and downstream phases can reference `Job.Conclusion` without further struct changes.

#### 0.2 `internal/store/store.go`

Add the methods the worker and webhook need but the current interface doesn't expose:

```go
GetJob(ctx context.Context, jobID int64) (*Job, error)
MarkJobInProgress(ctx context.Context, jobID int64, runnerID int64, runnerName string) error
MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) error
```

Remove the polymorphic `UpdateJobStatus(...)` — splitting matches the two real
call sites (webhook on `in_progress`, webhook on `completed`).

Canonical names for the rest of the interface (any older draft that disagrees
is wrong):

| Method | Purpose |
|---|---|
| `SaveAppConfig` / `GetAppConfig` | App credentials (singleton row) |
| `UpsertInstallation` / `ListInstallations` | Per-installation rows |
| `UpsertRepoInstallation(repo, installationID)` / `RemoveRepoInstallation(repo)` / `InstallationForRepo(repo)` | Repo↔installation mapping |
| `InsertJobIfNew(j) (inserted bool, err error)` | Dedupe guard via `INSERT OR IGNORE` |
| `GetJob`, `MarkJobInProgress`, `MarkJobCompleted`, `PendingJobs` | Job state machine |
| `InsertRunner` / `UpdateRunnerStatus(containerName)` / `UpdateRunnerStatusByName(runnerName)` / `ActiveRunnerCount` / `ListActiveRunners` | Runner lifecycle. Scheduler uses by-container-name (it just spawned it); webhook uses by-runner-name (the only id GitHub gives it). |

**Test plan:**
- `go build ./...` green (sqlite stub still satisfies the interface — update the stub method set in the same commit).
- `go vet ./...` clean.
- `grep -r "UpdateJobStatus"` returns no hits — the rename is total.
- **Done when:** `go build ./...` passes with zero references to the removed signature.

**Phase 0 commit:** `refactor(store): split UpdateJobStatus, add GetJob, Conclusion field`.

---

### Phase 1 — `internal/store/sqlite.go` (foundation)

**Goal:** real persistence. Everything downstream depends on it. Driver:
`modernc.org/sqlite` (pure Go, no CGO). Add to `go.mod`.

#### 1.1 Schema (embedded SQL, applied at `OpenSQLite` time)

```sql
CREATE TABLE IF NOT EXISTS app_config (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
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
  id             INTEGER PRIMARY KEY,
  account_id     INTEGER NOT NULL,
  account_login  TEXT    NOT NULL,
  account_type   TEXT    NOT NULL,
  created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Repo↔installation mapping. Populated from `installation` (created) and
-- `installation_repositories` (added/removed) webhook events; also from the
-- top-level `installation.id` field on `workflow_job` payloads as a lazy
-- fallback (so a missed installation event never permanently strands a repo).
CREATE TABLE IF NOT EXISTS installation_repos (
  repo            TEXT    PRIMARY KEY,           -- "owner/repo"
  installation_id INTEGER NOT NULL,
  added_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (installation_id) REFERENCES installations(id)
);
CREATE INDEX IF NOT EXISTS idx_installation_repos_inst ON installation_repos(installation_id);

CREATE TABLE IF NOT EXISTS jobs (
  id           INTEGER PRIMARY KEY,
  repo         TEXT    NOT NULL,
  action       TEXT    NOT NULL,
  labels       TEXT    NOT NULL,
  dedupe_key   TEXT    NOT NULL UNIQUE,
  status       TEXT    NOT NULL,
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
  runner_name    TEXT    NOT NULL,
  labels         TEXT    NOT NULL,
  status         TEXT    NOT NULL,
  started_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  finished_at    DATETIME
);
CREATE INDEX IF NOT EXISTS idx_runners_status ON runners(status);
```

Both `container_name` and `runner_name` from day one — v1.1 reconciliation
joins on container name (to `docker inspect`) and runner name (to GitHub
`/runners`). Backfilling later is impossible.

**Test plan (`schema_test.go`):**
- Open in-memory DSN (`file::memory:?cache=shared`), assert all 5 tables exist via `sqlite_master`.
- Re-`OpenSQLite` against the same DSN — schema apply is idempotent (no `table already exists`).
- **Done when:** schema is byte-for-byte identical across two opens.

#### 1.2 Methods (`sqlite.go`)

Implementation notes:
- All queries use `db.QueryContext` / `ExecContext`.
- `InsertJobIfNew`: `INSERT … ON CONFLICT(dedupe_key) DO NOTHING`; check `RowsAffected` for the `inserted bool`.
- `ActiveRunnerCount`: `SELECT count(*) FROM runners WHERE status IN ('starting','idle','busy')`.
- `MarkJobInProgress` / `MarkJobCompleted` bump `updated_at = CURRENT_TIMESTAMP`.
- `Close()` closes the underlying `*sql.DB`.

**Test plan (`sqlite_test.go`)** — one test per method, in-memory DSN per test:

| Test | Setup | Assertion |
|---|---|---|
| `TestSaveAndGetAppConfig` | call `SaveAppConfig(cfg)` | `GetAppConfig` returns equal struct |
| `TestUpsertInstallation_Idempotent` | upsert same row twice | `ListInstallations` length == 1 |
| `TestUpsertRepoInstallation_OverwritesOnReinstall` | upsert `repo→inst1`, then `repo→inst2` | `InstallationForRepo("repo")` returns inst2 |
| `TestRemoveRepoInstallation` | upsert then remove | `InstallationForRepo` returns nil |
| `TestInsertJobIfNew_DupReturnsFalse` | insert same dedupe_key twice | first `inserted=true`, second `inserted=false`, no error |
| `TestMarkJobInProgressThenCompleted` | insert job, mark in_progress, mark completed | `GetJob` reflects status, runner_id, runner_name, conclusion |
| `TestPendingJobs_Order` | insert 3 jobs with different `received_at` | returned in `received_at ASC` |
| `TestActiveRunnerCount_StatusFilter` | insert runners across all 4 statuses | count == 3 (starting/idle/busy only) |
| `TestUpdateRunnerStatus_FinishedSetsFinishedAt` | insert runner, mark finished | `finished_at IS NOT NULL` |
| `TestUpdateRunnerStatusByName_MatchesRunnerNameNotContainer` | insert runner with distinct container_name and runner_name; call by runner_name | row's status updated, container_name lookup also reflects it |

Manual smoke: `docker compose up`, see `OpenSQLite` log line, see file created at the configured DSN path.

**Done when:** `go test ./internal/store/...` green AND smoke passes.

**Phase 1 commits:** `feat(store): sqlite schema + migrations`, `feat(store): implement Store methods`, `test(store): per-method coverage`.

---

### Phase 2 — `internal/github/*` (API client) ‖ parallelizable across files

**Goal:** every GitHub call the rest of the system needs. No retry/backoff yet.

#### 2.1 `auth.go`

```go
func (c *Client) AppJWT(ctx context.Context, st store.Store) (string, error)
func (c *Client) InstallationToken(ctx context.Context, st store.Store, installationID int64) (string, error)
```

- JWT signer: `github.com/golang-jwt/jwt/v5`. RS256, `iat = now-60s`, `exp = now+10m`, `iss = app_id`.
- Installation-token cache: `sync.Map` keyed by `int64` → `struct{token string; exp time.Time}`. TTL = `expires_at - 5min`.
- Endpoint: `POST {api}/app/installations/{id}/access_tokens` with `Authorization: Bearer <appJWT>`.

**Test plan (`auth_test.go`):**
| Test | Approach |
|---|---|
| `TestAppJWT_HeaderClaims` | mint, decode without verify, assert `alg=RS256`, `iss=app_id`, `exp-iat ≈ 10m` |
| `TestInstallationToken_CachesUntilExpiry` | `httptest` server returning expiry=`now+1h`; first call hits, second within window does not (assert request count) |
| `TestInstallationToken_RefreshAfterMargin` | freeze clock, advance past `exp - 5min`, assert second call re-hits the server |

**Done when:** all three pass; manual JWT decoded at `jwt.io` against the test pem matches.

#### 2.2 `manifest.go::ConvertCode`

```go
func (c *Client) ConvertCode(ctx context.Context, code string) (*AppCredentials, error)
```

`POST https://api.github.com/app-manifests/<code>/conversions`. No auth header. Decode JSON; `pem` (string) → `[]byte`. No retry.

**Test plan (`manifest_test.go`):**
| Test | Approach |
|---|---|
| `TestConvertCode_HappyPath` | `httptest` server returns canned JSON; assert all 6 fields populated |
| `TestConvertCode_NonJSONFails` | server returns HTML; assert error mentions decode |
| `TestConvertCode_404Surfaces` | server 404s; assert error includes status |
| `TestBuildManifest_FieldsFromBaseURL` | already done — re-confirm `hook_attributes.url` and `redirect_url` derive from `BaseURL` |

**Done when:** `go test ./internal/github -run Manifest` green.

#### 2.3 `runners.go::RegistrationToken`

```go
func (c *Client) RegistrationToken(ctx context.Context, owner, repo, installToken string) (string, error)
```

`POST {api}/repos/{owner}/{repo}/actions/runners/registration-token`, `Authorization: Bearer <installToken>`. Return `token` only. Never cache.

`ListRepoRunners` and `DeleteRepoRunner` deferred to v1.1.

**Test plan (`runners_test.go`):**
| Test | Approach |
|---|---|
| `TestRegistrationToken_HappyPath` | `httptest` server returns `{"token":"abc","expires_at":...}`; assert returned == "abc" |
| `TestRegistrationToken_AuthHeaderForwarded` | server inspects `Authorization`; asserts `Bearer <installToken>` |
| `TestRegistrationToken_NoCache` | call twice; assert server received 2 requests |

**Done when:** `go test ./internal/github -run Registration` green.

**Phase 2 commits:** `feat(github): app JWT + installation token cache`, `feat(github): manifest ConvertCode`, `feat(github): registration token`.

---

### Phase 3 — `internal/httpapi/handlers/*` ‖ parallelizable across handlers

Depends on Phase 1 (store) + Phase 2 (github client). Three handlers, independent.

#### 3.1 `setup.go` — `GET /setup`

1. `st.GetAppConfig`: if found and `BaseURL` matches `cfg.BaseURL`, render `setup_done.html` with install link `https://github.com/apps/<slug>/installations/new`.
2. Generate state: `crypto/rand` 16 bytes → hex.
3. Set cookie `gharp_state` (HttpOnly, Secure if BaseURL is https, SameSite=Lax, 10-min expiry).
4. Build manifest: `github.BuildManifest(cfg.BaseURL)` → JSON-encode → embed in `setup.html`. Form `action` = `https://github.com/settings/apps/new?state=<state>`, method `POST`.

**Test plan (`setup_test.go`):**
| Test | Approach |
|---|---|
| `TestSetup_FreshInstall_RendersForm` | empty store; `httptest` GET; assert `Set-Cookie: gharp_state=`, body contains `manifest=` and `action="https://github.com/settings/apps/new?state=`|
| `TestSetup_ConfiguredInstall_RendersInstallLink` | store has matching app_config; assert body contains `apps/<slug>/installations/new` |
| `TestSetup_BaseURLMismatch_RendersForm` | store has app_config with old base_url; behaves like fresh (lets user re-bootstrap) |
| `TestSetup_StateCookieIsHTTPOnly` | parse `Set-Cookie`; assert `HttpOnly` flag, `MaxAge ≈ 600` |

**Done when:** all 4 pass.

#### 3.2 `callback.go` — `GET /github/app/callback?code=…&state=…`

1. Read `gharp_state` cookie. `subtle.ConstantTimeCompare` against `state` query. Mismatch → 400.
2. `gh.ConvertCode(ctx, code)` (no queueing).
3. `st.SaveAppConfig(...)` with returned credentials + `cfg.BaseURL`.
4. Clear `gharp_state` cookie (`MaxAge=-1`).
5. Render `setup_done.html` with install link.

**Test plan (`callback_test.go`):** uses a fake `gh` interface returning `AppCredentials`.
| Test | Approach |
|---|---|
| `TestCallback_StateMismatch_400` | cookie="A", query state="B" → 400 |
| `TestCallback_NoCookie_400` | no cookie → 400 |
| `TestCallback_HappyPath_SavesAndRenders` | matching state; assert fake `SaveAppConfig` called with decoded creds; body contains install link |
| `TestCallback_ConvertCodeFails_500WithLog` | fake returns error; assert 500, no `SaveAppConfig` call |
| `TestCallback_ClearsStateCookie` | response sets `gharp_state=; Max-Age=0` |

**Done when:** all 5 pass.

#### 3.3 `webhook.go` — `POST /github/webhook`

Hard ceiling: ≤ ~3s typical, < 10s worst. **No GitHub API calls, no `docker run`.**

Order (cannot reorder):

1. Read raw body once into `[]byte`.
2. Verify HMAC of `X-Hub-Signature-256`: `hmac.Equal([]byte(received), []byte("sha256="+hex.EncodeToString(mac.Sum(nil))))`. Mismatch → 401.
3. Switch on `X-GitHub-Event`:

   | Event | Action |
   |---|---|
   | `installation` (`created`) | `UpsertInstallation`; `UpsertRepoInstallation` for every repo in `repositories` |
   | `installation` (`deleted`) | (v1) log only; entries become stale but `workflow_job` lazy-write below will not refresh them, and the registration-token call will 404 — surfaced via worker logs |
   | `installation_repositories` (`added`) | `UpsertRepoInstallation` for each repo in `repositories_added` |
   | `installation_repositories` (`removed`) | `RemoveRepoInstallation` for each repo in `repositories_removed` |
   | `workflow_job` | See §3.3.1. Also: lazy-write `UpsertRepoInstallation(repo, payload.installation.id)` so a missed installation event never strands the repo. |
   | anything else | 200, no-op |

##### 3.3.1 `workflow_job` handling

- Parse into `scheduler.WorkflowJobEvent`.
- **Label filter (early!):** intersect `payload.workflow_job.labels` with `cfg.RunnerLabels`. Empty intersection → 200, do not insert. Skip when `RunnerLabels` is unset.
- Branch:

  | action | Steps |
  |---|---|
  | `queued` | `inserted, err := st.InsertJobIfNew(j)`; on `err` return 5xx (let GitHub retry — losing a queued event silently means the job never runs); if `inserted`, `sch.Enqueue(j.ID)` |
  | `in_progress` | `MarkJobInProgress(jobID, runnerID, runnerName)` + `UpdateRunnerStatusByName(runnerName,"busy")` (best-effort — bookkeeping only; `err` is logged but does not change response) |
  | `completed` | `MarkJobCompleted(jobID, conclusion)` + `UpdateRunnerStatusByName(runnerName,"finished")` (same: best-effort bookkeeping) |

- Response code policy:
  - 401 — HMAC mismatch.
  - 400 — body unparseable.
  - **5xx — store error on the `queued` path.** GitHub will retry; the
    `INSERT OR IGNORE` dedupe guard makes retry safe. Returning 2xx here
    permanently drops the event.
  - 200 — everything else (including bookkeeping store errors on
    `in_progress` / `completed`, which only affect the dashboard, not
    correctness).

**Test plan (`webhook_test.go`):** in-memory store + spy scheduler.
| Test | Approach |
|---|---|
| `TestWebhook_BadSignature_401` | wrong secret → 401, no store writes |
| `TestWebhook_GoodSignature_200` | correct HMAC → 200 |
| `TestWebhook_QueuedDuplicate_DedupedAtStore` | deliver same `workflow_job.id` twice; assert `Enqueue` called once |
| `TestWebhook_QueuedStoreError_Returns5xx` | `InsertJobIfNew` returns error; assert response 5xx and `Enqueue` NOT called (so GitHub retries) |
| `TestWebhook_LabelFilter_DropsNonMatching` | runs-on=`["foo"]`, RunnerLabels=`["bar"]` → 200, no `InsertJobIfNew` |
| `TestWebhook_LabelFilter_EmptyConfigPasses` | RunnerLabels nil → insert proceeds |
| `TestWebhook_InProgress_BindsRunner` | assert `MarkJobInProgress` called with `runnerID`/`runnerName` from payload |
| `TestWebhook_Completed_RecordsConclusion` | assert `MarkJobCompleted` called with payload conclusion |
| `TestWebhook_InstallationCreated_Upserts` | assert `UpsertInstallation` called |
| `TestWebhook_UnknownEvent_200NoOp` | event `push` → 200, no calls |

**Done when:** all 9 pass; integration smoke (Phase 5) confirms end-to-end.

**Phase 3 commits:** `feat(handlers): setup`, `feat(handlers): callback`, `feat(handlers): webhook + label filter + dedupe`.

---

### Phase 4 — `internal/scheduler.Run` (close the loop)

**Goal:** make the hot path actually spawn containers.

#### 4.1 Startup replay

```go
pending, _ := s.store.PendingJobs(ctx)
for _, j := range pending { s.Enqueue(j.ID) }
```

Channel is 256-buffered; overflow stays `pending` for next restart.

**Test plan (`scheduler_replay_test.go`):**
| Test | Approach |
|---|---|
| `TestRun_ReplayPendingOnStartup` | seed 3 pending jobs; start Run; assert 3 dispatch calls observed |
| `TestRun_ReplayOverflow_LeavesPending` | seed 300 pending; assert no panic and ≤ 256 enqueued before first dispatch drains |

#### 4.2 Worker loop

```go
for {
  select {
  case <-ctx.Done(): return ctx.Err()
  case jobID := <-s.jobCh: s.dispatch(ctx, jobID)
  }
}
```

`dispatch(ctx, jobID)`:

1. **Concurrency cap (before any API call).** `n, _ := s.store.ActiveRunnerCount(ctx)`; if `n >= cfg.MaxConcurrentRunners`, sleep ~2s, `s.Enqueue(jobID)`, return.
2. `job := s.store.GetJob(ctx, jobID)` (skip if status != `pending`).
3. `inst := s.store.InstallationForRepo(ctx, job.Repo)`. If nil → log, leave pending.
4. `installToken := s.gh.InstallationToken(ctx, s.store, inst.ID)`.
5. `owner, repo := split(job.Repo)`; `regToken := s.gh.RegistrationToken(...)`.
6. Insert `Runner` row with `Status:"starting"`, both `ContainerName` and `RunnerName` populated.
7. `s.runner.Launch(ctx, runner.Spec{...})`. On error: `UpdateRunnerStatus(containerName, "finished")` and log.

**Test plan (`scheduler_dispatch_test.go`)** — all use a `Launcher` interface mocked to no-op + spy github:
| Test | Approach |
|---|---|
| `TestDispatch_ConcurrencyCap_Requeues` | seed cap-1 active runners; enqueue 1 job; spy: `RegistrationToken` NOT called; channel re-receives the same id |
| `TestDispatch_NoInstallation_StaysPending` | repo with no installation row; assert no token mint; `GetJob` still pending |
| `TestDispatch_HappyPath_InsertsStartingRunner` | full path; assert `runners` table has row with `status="starting"`, both names set, `Launch` called once |
| `TestDispatch_LaunchError_MarksFinished` | mock launcher returns error; assert runner row `status="finished"` |
| `TestDispatch_TokenOrder` | spy verifies `ActiveRunnerCount` called BEFORE `InstallationToken` (cap-before-mint invariant) |

**Done when:** all 7 pass + manual smoke: signed `workflow_job: queued` POST → `runners` row appears in `starting`.

**Phase 4 commit:** `feat(scheduler): startup replay + dispatch loop`.

---

### Phase 5 — Startup validation, config additions, polish

#### 5.1 `config.RunnerLabels []string` (env `RUNNER_LABELS`, comma-separated)

Empty = serve all. Drives the webhook label filter.

**Test plan:**
- `TestLoad_RunnerLabels_Parse` — `RUNNER_LABELS=foo,bar`; assert `[]string{"foo","bar"}`.
- `TestLoad_RunnerLabels_EmptyDefaultsNil` — unset; assert `nil` (not `[]string{}`).

#### 5.2 `config.GitHubAPIBase string` (env `GITHUB_API_BASE`, default `https://api.github.com`)

For GHES support; threaded through `internal/github/*`.

**Test plan:**
- `TestLoad_GitHubAPIBase_Default` — unset → `https://api.github.com`.
- `TestLoad_GitHubAPIBase_OverrideRespected` — `GITHUB_API_BASE=https://gh.example.com/api/v3` → echoed.
- Add an `auth_test.go` case asserting `httptest` server URL is honored when threaded as `GitHubAPIBase`.

#### 5.3 BaseURL drift warning at boot (`cmd/gharp/main.go`)

After `OpenSQLite`: `if existing, _ := st.GetAppConfig(ctx); existing != nil && existing.BaseURL != cfg.BaseURL { log.Warn(...) }`. Don't block startup.

**Test plan:**
- `TestMain_BaseURLDrift_Warns` — table-driven on a small extracted helper `checkBaseURLDrift(existing *AppConfig, configured string) (warn bool, msg string)`. Cases: nil → no warn; equal → no warn; different → warn with both URLs in message.
- Manual: change `BASE_URL` in `.env` between runs; confirm warning line in stderr.

#### 5.4 `/setup` always shows install link when configured

When both `app_config` and ≥1 `installations` row exist, `/setup` still renders the install link (so user can add another installation).

**Test plan:** add a case to `TestSetup_ConfiguredInstall_RendersInstallLink` (Phase 3) for "configured + has installations" — same expected output.

#### 5.5 README / `.env.example` updated

Document `RUNNER_LABELS`, `GITHUB_API_BASE`, BaseURL-drift caveat.

**Test plan:** human read-through; `grep "RUNNER_LABELS" README.md .env.example` non-empty.

#### 5.6 Manual end-to-end (release gate)

Real ngrok URL, throwaway repo, real workflow.

**Steps & assertions:**
1. Setup flow: `/setup` → GitHub → callback → `setup_done`. Assert `app_config` row exists.
2. Install on the repo. Assert `installation: created` webhook landed and `installations` row created.
3. Push a workflow with `runs-on: self-hosted` matching `RUNNER_LABELS`. Assert:
   - `jobs` row inserted with `status=pending` then `in_progress` then `completed`.
   - `runners` row through `starting → busy → finished`.
   - Container visible in `docker ps -a` with the configured `--name`.
   - Workflow succeeds in GitHub UI.
4. Push a workflow with non-matching `runs-on`. Assert `jobs` table unchanged.

**Done when:** all six items shipped + smoke #6 passes.

**Phase 5 commits:** `feat(config): RUNNER_LABELS, GITHUB_API_BASE`, `feat(main): BASE_URL drift warning`, `docs: env vars + quickstart`.

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

## PR layout

One PR per phase, each branched off `main` after the prior one merges. Branch
names are assigned at implementation time (not pre-allocated here):

- **This PR** — `docs/components.md` rewrite **+ Phase 0** (Store interface
  tweak + `Job.Conclusion`). Phase 0 is bundled because it's a ~15-line
  interface change inseparable from the doc that defines it.
- Phase 1 — sqlite.go + tests
- Phase 2 — github/{auth,manifest,runners}
- Phase 3 — handlers/{setup,callback,webhook}
- Phase 4 — scheduler.Run dispatch + replay
- Phase 5 — RUNNER_LABELS, BASE_URL drift warning, README, end-to-end smoke

## Critical invariants (re-stated for the implementer)

These come from `architecture.md` §"Critical invariants" and bind every phase:

- HMAC over the **raw body**, before any JSON parse.
- Webhook handler returns < 10s — only steps 1-5, never a `docker run` or GitHub API call.
- Dedupe by `workflow_job.id` via `INSERT … ON CONFLICT DO NOTHING`. Channel dedup is not authoritative.
- sqlite is the durable record; channel is best-effort. Replay `PendingJobs` on startup.
- Registration tokens are single-use; never cache.
- Installation tokens cached in-memory for ~55 min (1h TTL − 5 min margin).
- Concurrency cap checked **before** minting any token.
- Filter on `runs-on` labels at the webhook, not in the worker.
- Always store `container_name` AND `runner_name` on the `runners` row from v1 — v1.1 reconciliation needs both.
