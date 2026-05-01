// Package reconciler runs a periodic loop that joins the runners table
// against actual docker state. It exists to break the cap deadlock that
// happens when the runners table reports active rows but the
// corresponding containers are gone (process crashed mid-launch, host
// rebooted, someone manually `docker rm`ed): without intervention,
// ActiveRunnerCount stays at the cap forever and dispatch stalls.
//
// Scope (intentionally narrow for the first cut):
//
//  1. Ghost runner: row in ('starting','idle','busy') but `docker
//     inspect` reports the container is gone. Mark the row 'finished'.
//     The dispatched job tied to that runner is left for the scheduler's
//     existing dispatchedReplayAge replay to rescue.
//
//  2. Orphan container: a container whose name matches our prefix
//     exists (any state), but no active runner row claims it. Force-remove it.
//     A grace window protects very new containers from being swept
//     during transient docker-vs-DB visibility skew across separate
//     SQLite connections (InsertRunner commits on the writer
//     connection BEFORE `docker run` is invoked, but a different
//     reader connection in the pool may not see that row yet for a
//     few hundred ms). Also covers the rare case where a `gharp-`
//     named container appears via a non-standard launch path.
//
//  3. GitHub-side ghost runner: a runner is registered with GitHub
//     under our `containerNamePrefix` namespace but no active row in
//     our table claims it. Force-deregister via DELETE /actions/
//     runners/{id}. Runs on a slower 5-minute cadence to bound
//     install-token quota usage. Skipped entirely when the
//     Reconciler was constructed with a nil GitHubClient.
//
//  4. Stale in_progress job: an `in_progress` row whose `updated_at`
//     is older than maxLifetime — i.e. the `workflow_job: completed`
//     webhook never arrived. Replay only covers pending + stale
//     dispatched, so without this sweep these rows would never
//     transition. Same cadence and install-token cache as (3); skipped
//     when gh is nil or maxLifetime is zero.
//
// Out of scope for this pass: idle-timeout reaping, dispatch-time
// live cap checks. See docs/architecture.md §"Ghost runners and
// reconciliation".
package reconciler

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// DefaultContainerNamePrefix is the production default for the
// orphan-sweep namespace. The actual prefix in use is the per-
// Reconciler `containerNamePrefix` (set from cfg.RunnerNamePrefix at
// construction time) — anything not starting with that prefix is
// left alone, so a deployment can run alongside other gharp pools
// or unrelated containers on the same docker daemon without the
// reconciler reaching into them.
const DefaultContainerNamePrefix = "gharp-"

// Docker is the subset of docker CLI behavior the reconciler depends
// on. Kept as an interface so tests can fake it without spawning real
// containers.
type Docker interface {
	// Inspect returns whether a container with the given name currently
	// exists (any state). The reconciler only cares about
	// existence/non-existence; status detail is a later concern.
	Inspect(ctx context.Context, containerName string) (exists bool, err error)
	// ListByPrefix returns containers whose name begins with the given
	// prefix, regardless of state, with their docker-reported creation
	// timestamps. The orphan sweep uses CreatedAt for per-container
	// grace gating so a steady stream of new dispatches can't
	// indefinitely defer cleanup of an actually-old orphan.
	ListByPrefix(ctx context.Context, prefix string) ([]ContainerInfo, error)
	// ForceRemove issues `docker rm -f` on the given container name. A
	// "no such container" outcome is treated as success.
	ForceRemove(ctx context.Context, containerName string) error
}

// ContainerInfo is what ListByPrefix returns per container. CreatedAt
// is the docker-reported creation time; if parsing failed it's the
// zero time and the orphan sweep treats the container as old enough
// to remove (better to over-clean an undatable orphan than to leak).
type ContainerInfo struct {
	Name      string
	CreatedAt time.Time
}

// Store is the subset of store.Store the reconciler needs. Defined here
// so tests can supply a thin fake without implementing every method.
type Store interface {
	ListActiveRunners(ctx context.Context) ([]*store.Runner, error)
	UpdateRunnerStatus(ctx context.Context, containerName, status string) error
	// Below: needed only by the GitHub-side ghost sweep. Optional in
	// the sense that a Reconciler with a nil GitHubClient will never
	// call these — passing a Store that returns errors here is fine
	// when GitHub-side cleanup is disabled.
	GetAppConfig(ctx context.Context) (*store.AppConfig, error)
	// ListAllInstallationRepos drives the GitHub-side sweep. Iterating
	// every installed repo (rather than only repos with active rows)
	// catches ghosts left behind after a deployment goes idle — the
	// docker side of those ghosts is already cleaned up locally, so
	// without this iteration GitHub would hold the registration
	// until its own ~30-day timeout.
	ListAllInstallationRepos(ctx context.Context) ([]store.RepoInstallation, error)
	// ListJobs + MarkJobCompleted: needed by the stale-in_progress
	// sweep, which catches rows whose `workflow_job: completed`
	// webhook never arrived (delivery failure, signature mismatch,
	// process downtime mid-event). The sweep filters in memory by
	// updated_at and writes the GitHub-side authoritative conclusion.
	ListJobs(ctx context.Context, f store.JobListFilter) ([]*store.Job, error)
	MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) (bool, error)
}

// GitHubClient is the subset of *github.Client the GitHub-side ghost
// sweep depends on. nil disables that sweep entirely (the docker-side
// sweeps still run on the normal cadence).
type GitHubClient interface {
	AppJWT(pem []byte, appID int64) (string, error)
	InstallationToken(ctx context.Context, jwt string, installationID int64) (string, error)
	ListRepoRunners(ctx context.Context, installationToken, repoFullName string) ([]github.RepoRunner, error)
	DeleteRepoRunner(ctx context.Context, installationToken, repoFullName string, runnerID int64) error
	// WorkflowJob is the source-of-truth read used by the
	// stale-in_progress sweep when a webhook completion event is
	// presumed lost.
	WorkflowJob(ctx context.Context, installationToken, repoFullName string, jobID int64) (*github.WorkflowJobStatus, error)
}

// Reconciler reconciles the local runner table and container state with GitHub's runner registry.
type Reconciler struct {
	store               Store
	docker              Docker
	gh                  GitHubClient // nil disables the GitHub-side sweep
	log                 *slog.Logger
	period              time.Duration
	githubSweepPeriod   time.Duration
	grace               time.Duration
	maxLifetime         time.Duration
	containerNamePrefix string
	workdirRoot         string
	workdirSweepPeriod  time.Duration
	workdirGrace        time.Duration
	// maintenanceCmd and maintenancePeriod drive the optional periodic
	// user-supplied maintenance command (e.g. docker system prune).
	// Both must be non-zero to enable the goroutine.
	maintenanceCmd    []string
	maintenancePeriod time.Duration
	nowFn             func() time.Time
}

// New creates a new Reconciler with the specified configuration.
// maxLifetime caps how long a runner can stay active before force-removal.
// containerNamePrefix scopes the orphan sweep (empty falls back to DefaultContainerNamePrefix).
// gh is optional — pass nil to skip GitHub-side ghost-runner sweep.
// workdirRoot enables filesystem cleanup (empty disables it).
// maintenanceCmd with maintenancePeriod enables periodic user-supplied maintenance.
func New(st Store, dk Docker, gh GitHubClient, log *slog.Logger, maxLifetime time.Duration, containerNamePrefix, workdirRoot string, maintenanceCmd []string, maintenancePeriod time.Duration) *Reconciler {
	if containerNamePrefix == "" {
		containerNamePrefix = DefaultContainerNamePrefix
	}
	workdirRoot = strings.TrimSpace(workdirRoot)
	return &Reconciler{
		store:  st,
		docker: dk,
		gh:     gh,
		log:    log,
		period: 60 * time.Second,
		// GitHub /runners is rate-limited (5000 req/h per install
		// token) and most ghost rows already get cleared by the
		// docker-side sweep on the normal cadence; running this
		// every 5 ticks (5min) is plenty.
		githubSweepPeriod: 5 * time.Minute,
		// Orphan grace: protect very new containers from sweep during
		// short-lived docker-vs-DB visibility skew. InsertRunner
		// commits on the writer connection BEFORE the launcher's
		// `docker run` is invoked, but a different reader connection
		// in the database/sql pool may briefly not see that row. 30s
		// is well over typical skew.
		grace:               30 * time.Second,
		maxLifetime:         maxLifetime,
		containerNamePrefix: containerNamePrefix,
		workdirRoot:         workdirRoot,
		workdirSweepPeriod:  5 * time.Minute,
		workdirGrace:        5 * time.Minute,
		maintenanceCmd:      maintenanceCmd,
		maintenancePeriod:   maintenancePeriod,
		nowFn:               time.Now,
	}
}

// Run blocks until ctx is cancelled, then returns ctx.Err().
// Runs docker-side sweep on the main loop (owns cap-deadlock cleanup).
// GitHub-side sweep runs in its own goroutine to avoid blocking docker cleanup.
// Non-context errors are logged and not propagated.
func (r *Reconciler) Run(ctx context.Context) error {
	r.Reconcile(ctx)
	if r.gh != nil {
		go r.runGitHubSweeper(ctx)
	}
	if r.workdirRoot != "" {
		go r.runWorkdirSweeper(ctx)
	}
	if len(r.maintenanceCmd) > 0 && r.maintenancePeriod > 0 {
		go r.runMaintenanceSweeper(ctx)
	}
	t := time.NewTicker(r.period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.Reconcile(ctx)
		}
	}
}

func (r *Reconciler) runWorkdirSweeper(ctx context.Context) {
	r.sweepOrphanWorkdirs(ctx)
	t := time.NewTicker(r.workdirSweepPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweepOrphanWorkdirs(ctx)
		}
	}
}

func (r *Reconciler) runGitHubSweeper(ctx context.Context) {
	r.sweepGitHubGhostRunners(ctx)
	r.sweepStaleInProgressJobs(ctx)
	t := time.NewTicker(r.githubSweepPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweepGitHubGhostRunners(ctx)
			r.sweepStaleInProgressJobs(ctx)
		}
	}
}

// Reconcile runs one full sweep: ghost runners, then orphan containers.
// Exposed for tests and for callers that want to force a sweep (e.g.
// after a crash recovery). Logs a debug heartbeat at the end so an
// operator tailing logs can confirm the loop is alive even when no
// action was taken (the steady state).
func (r *Reconciler) Reconcile(ctx context.Context) {
	known, ghostFinished, lifetimeReaped := r.sweepGhostRunners(ctx)
	orphans := r.sweepOrphanContainers(ctx, known)
	r.log.Debug("reconciler: tick complete",
		"active", len(known),
		"ghosts_cleared", ghostFinished,
		"lifetime_reaped", lifetimeReaped,
		"orphans_removed", orphans,
	)
}

// sweepGhostRunners walks ListActiveRunners and either:
//   - marks the row 'finished' if `docker inspect` cannot find the
//     container (the original ghost case — frees the cap slot
//     immediately), or
//   - force-removes the container AND marks the row finished if the
//     row's StartedAt is older than maxLifetime (the timeout case —
//     defends against EPHEMERAL runners that registered but never
//     claimed a job).
//
// Returns the set of container names left alive, so the orphan sweep
// doesn't have to re-inspect them.
func (r *Reconciler) sweepGhostRunners(ctx context.Context) (alive map[string]struct{}, ghostFinished, lifetimeReaped int) {
	alive = map[string]struct{}{}
	rows, err := r.store.ListActiveRunners(ctx)
	if err != nil {
		r.log.Error("reconciler: ListActiveRunners failed", "err", err)
		return alive, 0, 0
	}
	now := r.nowFn()
	for _, row := range rows {
		exists, err := r.docker.Inspect(ctx, row.ContainerName)
		if err != nil {
			// Conservative: a flaky docker daemon shouldn't cause us to
			// mark live runners finished. Skip and retry next tick.
			r.log.Warn("reconciler: docker inspect failed; leaving row alone", "container", row.ContainerName, "err", err)
			alive[row.ContainerName] = struct{}{}
			continue
		}
		if !exists {
			// Ghost: container vanished. Free the cap slot. The
			// dispatched job (if any) is left for scheduler.replay to
			// rescue after dispatchedReplayAge.
			if err := r.store.UpdateRunnerStatus(ctx, row.ContainerName, "finished"); err != nil {
				r.log.Error("reconciler: UpdateRunnerStatus(finished) failed", "container", row.ContainerName, "err", err)
				continue
			}
			r.cleanupRunnerWorkdir(row.ContainerName)
			r.log.Info("reconciler: ghost runner cleared", "container", row.ContainerName, "runner_name", row.RunnerName, "repo", row.Repo)
			ghostFinished++
			continue
		}
		// Container is alive. Lifetime check: a runner that's been
		// 'starting'/'idle'/'busy' past maxLifetime is force-reaped.
		// In normal operation an EPHEMERAL container exits well within
		// the lifetime window, so this path only fires for stuck or
		// never-claimed runners.
		age := now.Sub(row.StartedAt)
		if r.maxLifetime > 0 && age > r.maxLifetime {
			r.log.Info("reconciler: runner exceeded max lifetime; force-removing",
				"container", row.ContainerName, "runner_name", row.RunnerName, "repo", row.Repo, "age", age, "max_lifetime", r.maxLifetime)
			if err := r.docker.ForceRemove(ctx, row.ContainerName); err != nil {
				// Don't mark finished if we couldn't kill the container —
				// that would let the cap slot free up while the
				// container is still really running and could re-claim
				// jobs we don't know about.
				r.log.Error("reconciler: ForceRemove (lifetime) failed; leaving row alone", "container", row.ContainerName, "err", err)
				alive[row.ContainerName] = struct{}{}
				continue
			}
			if err := r.store.UpdateRunnerStatus(ctx, row.ContainerName, "finished"); err != nil {
				r.log.Error("reconciler: UpdateRunnerStatus(finished) after lifetime reap failed", "container", row.ContainerName, "err", err)
			}
			r.cleanupRunnerWorkdir(row.ContainerName)
			lifetimeReaped++
			continue
		}
		alive[row.ContainerName] = struct{}{}
	}
	return alive, ghostFinished, lifetimeReaped
}

// sweepOrphanContainers force-removes containers matching our name
// prefix that are not represented by an active runner row, AND whose
// own CreatedAt is past the grace window.
//
// The grace check is per-container, not per-host: an earlier version
// deferred the entire sweep whenever any active runner row was younger
// than the grace, which broke under continuous load — a host with a
// steady stream of new dispatches would never clean up actual orphans
// because there was always a young row in the table.
func (r *Reconciler) sweepOrphanContainers(ctx context.Context, known map[string]struct{}) (removed int) {
	cs, err := r.docker.ListByPrefix(ctx, r.containerNamePrefix)
	if err != nil {
		r.log.Error("reconciler: ListByPrefix failed", "err", err)
		return 0
	}
	if len(cs) == 0 {
		return 0
	}
	now := r.nowFn()
	for _, c := range cs {
		if !strings.HasPrefix(c.Name, r.containerNamePrefix) {
			continue // belt-and-suspenders; ListByPrefix already filtered
		}
		if _, ok := known[c.Name]; ok {
			continue
		}
		// Per-container grace: protect very new containers from sweep
		// during short-lived docker-vs-DB visibility skew across
		// SQLite connections. Zero CreatedAt (parse failure) is
		// treated as old — better to remove an undatable orphan than
		// to leak it forever.
		if !c.CreatedAt.IsZero() && now.Sub(c.CreatedAt) < r.grace {
			r.log.Debug("reconciler: deferring orphan removal during grace window", "container", c.Name, "age", now.Sub(c.CreatedAt))
			continue
		}
		if err := r.docker.ForceRemove(ctx, c.Name); err != nil {
			r.log.Error("reconciler: ForceRemove failed", "container", c.Name, "err", err)
			continue
		}
		r.cleanupRunnerWorkdir(c.Name)
		r.log.Info("reconciler: orphan container removed", "container", c.Name)
		removed++
	}
	return removed
}

func (r *Reconciler) cleanupRunnerWorkdir(containerName string) {
	if r.workdirRoot == "" {
		return
	}
	if !strings.HasPrefix(containerName, r.containerNamePrefix) {
		return
	}
	root := filepath.Clean(r.workdirRoot)
	target := filepath.Clean(filepath.Join(root, containerName))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		r.log.Warn("reconciler: refusing unsafe workdir cleanup target", "container", containerName, "target", target, "err", err)
		return
	}
	if err := os.RemoveAll(target); err != nil {
		r.log.Warn("reconciler: workdir cleanup failed", "container", containerName, "path", target, "err", err)
	}
}

func (r *Reconciler) sweepOrphanWorkdirs(ctx context.Context) (removed int) {
	if r.workdirRoot == "" {
		return 0
	}
	entries, err := os.ReadDir(r.workdirRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		r.log.Warn("reconciler: failed to list workdir root", "root", r.workdirRoot, "err", err)
		return 0
	}
	now := r.nowFn()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, r.containerNamePrefix) {
			continue
		}
		exists, err := r.docker.Inspect(ctx, name)
		if err != nil {
			r.log.Warn("reconciler: docker inspect failed during workdir sweep", "container", name, "err", err)
			continue
		}
		if exists {
			continue
		}
		info, err := e.Info()
		if err != nil {
			r.log.Warn("reconciler: failed to stat workdir entry", "container", name, "err", err)
			continue
		}
		if now.Sub(info.ModTime()) < r.workdirGrace {
			continue
		}
		r.cleanupRunnerWorkdir(name)
		removed++
	}
	if removed > 0 {
		r.log.Info("reconciler: orphan workdirs removed", "count", removed, "root", r.workdirRoot)
	}
	return removed
}

// sweepGitHubGhostRunners walks every repo the App is installed on
// and, for each one, calls GitHub's /actions/runners API. Any runner
// whose name starts with `containerNamePrefix` and isn't claimed by
// an active row in our DB is force-deregistered. This catches
// runners that:
//   - exited their EPHEMERAL job cleanly but `--rm` removed the
//     container before our completed webhook updated the DB (the
//     usual cause of GitHub-side ghost rows accumulating);
//   - we never persisted because of a process crash mid-launch;
//   - belong to long-since-removed deployments sharing this App.
//
// Iteration source is `ListAllInstallationRepos` (mirroring the
// docker prefix sweep's "list everything matching, ignore DB"
// approach) — limiting to repos with active rows would miss ghosts
// left behind after a deployment goes idle.
//
// Cost is bounded: one install-token mint + one GET /runners + one
// DELETE per ghost per unique repo, every githubSweepPeriod (default
// 5min). Errors at any stage are logged but don't stop the sweep —
// other repos still get processed.
func (r *Reconciler) sweepGitHubGhostRunners(ctx context.Context) {
	if r.gh == nil {
		return
	}
	repos, err := r.store.ListAllInstallationRepos(ctx)
	if err != nil {
		r.log.Error("reconciler/github: ListAllInstallationRepos failed", "err", err)
		return
	}
	if len(repos) == 0 {
		r.log.Debug("reconciler/github: no installed repos; skipping sweep")
		return
	}

	rows, err := r.store.ListActiveRunners(ctx)
	if err != nil {
		r.log.Error("reconciler/github: ListActiveRunners failed", "err", err)
		return
	}
	// Build (repo -> set of runner names we own from active rows).
	// Repos absent from `known` mean "no active runners" — for those
	// repos, every prefixed runner GitHub reports is a candidate for
	// deletion.
	known := map[string]map[string]struct{}{}
	for _, row := range rows {
		set, ok := known[row.Repo]
		if !ok {
			set = map[string]struct{}{}
			known[row.Repo] = set
		}
		set[row.RunnerName] = struct{}{}
	}

	appCfg, err := r.store.GetAppConfig(ctx)
	if err != nil {
		r.log.Error("reconciler/github: GetAppConfig failed; skipping sweep", "err", err)
		return
	}
	if appCfg == nil {
		r.log.Debug("reconciler/github: app config not set; skipping sweep")
		return
	}
	jwt, err := r.gh.AppJWT(appCfg.PEM, appCfg.AppID)
	if err != nil {
		r.log.Error("reconciler/github: AppJWT failed; skipping sweep", "err", err)
		return
	}

	// Per-installation token cache. Repos sharing an installation
	// share the same token — minting per repo would burn quota for
	// no benefit.
	tokens := map[int64]string{}
	totalDeleted := 0
	for _, ri := range repos {
		tok, ok := tokens[ri.InstallationID]
		if !ok {
			tok, err = r.gh.InstallationToken(ctx, jwt, ri.InstallationID)
			if err != nil {
				r.log.Error("reconciler/github: InstallationToken failed", "repo", ri.Repo, "installation_id", ri.InstallationID, "err", err)
				continue
			}
			tokens[ri.InstallationID] = tok
		}
		runners, err := r.gh.ListRepoRunners(ctx, tok, ri.Repo)
		if err != nil {
			r.log.Error("reconciler/github: ListRepoRunners failed", "repo", ri.Repo, "err", err)
			continue
		}
		ours := known[ri.Repo] // nil set is fine — no active rows for this repo
		for _, gr := range runners {
			// Only touch runners whose name matches our prefix —
			// otherwise we'd reach into other deployments sharing
			// the same App installation.
			if !strings.HasPrefix(gr.Name, r.containerNamePrefix) {
				continue
			}
			if _, mine := ours[gr.Name]; mine {
				continue
			}
			// Belt-and-suspenders: don't delete a runner GitHub
			// still says is busy. If it's busy with one of OUR
			// jobs, our DB has it; if it's busy with someone
			// else's job, deletion would interrupt them.
			if gr.Busy {
				r.log.Debug("reconciler/github: skipping busy ghost runner", "repo", ri.Repo, "runner", gr.Name)
				continue
			}
			stillActive, err := r.activeRunnerExists(ctx, ri.Repo, gr.Name)
			if err != nil {
				r.log.Error("reconciler/github: ListActiveRunners recheck failed; skipping delete", "repo", ri.Repo, "runner_name", gr.Name, "err", err)
				continue
			}
			if stillActive {
				r.log.Debug("reconciler/github: skipping runner found during active recheck", "repo", ri.Repo, "runner", gr.Name)
				continue
			}
			if err := r.gh.DeleteRepoRunner(ctx, tok, ri.Repo, gr.ID); err != nil {
				r.log.Error("reconciler/github: DeleteRepoRunner failed", "repo", ri.Repo, "runner_id", gr.ID, "runner_name", gr.Name, "err", err)
				continue
			}
			r.log.Info("reconciler/github: deregistered ghost runner", "repo", ri.Repo, "runner_id", gr.ID, "runner_name", gr.Name)
			totalDeleted++
		}
	}
	r.log.Debug("reconciler/github: sweep complete",
		"repos", len(repos), "deleted", totalDeleted)
}

func (r *Reconciler) activeRunnerExists(ctx context.Context, repo, runnerName string) (bool, error) {
	rows, err := r.store.ListActiveRunners(ctx)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Repo == repo && row.RunnerName == runnerName {
			return true, nil
		}
	}
	return false, nil
}

// staleInProgressListLimit caps the per-tick read size. In normal
// operation in_progress count is bounded by MaxConcurrentRunners
// (typically single digits), so 500 is wildly above any realistic
// candidate set even after a long webhook outage. Backlogs that
// somehow exceed this drain across multiple ticks — each sweep
// processes up to this many candidates, then stops; the next
// tick (5min later) picks up where we left off because ListJobs
// orders by updated_at DESC and the rows we already reconciled
// no longer match the in_progress filter.
const staleInProgressListLimit = 500

// sweepStaleInProgressJobs catches in_progress rows whose
// `workflow_job: completed` webhook never arrived (delivery failure,
// signature mismatch, process downtime mid-event). Without this,
// such rows stay in_progress forever — the existing replay only
// covers pending + stale dispatched, and webhook completion is the
// only writer that transitions in_progress → completed.
//
// Threshold reuses RunnerMaxLifetime: a runner can't outlive
// maxLifetime (the docker-side reconciler force-reaps past it), so
// any in_progress row whose updated_at is older than that has
// definitely lost its webhook.
//
// Cadence shares runGitHubSweeper (default 5min): same
// installation-token cache, same AppJWT mint, same per-installation
// quota budget as the GitHub-side ghost-runner sweep.
//
// Race: ListJobs → MarkJobCompleted is unguarded; a webhook landing
// in that window would have its conclusion overwritten. The window
// is one HTTP round-trip and the replacement value comes from the
// same GitHub API as the webhook payload, so the overwrite is
// benign. Skipped entirely when gh is nil or maxLifetime is zero.
func (r *Reconciler) sweepStaleInProgressJobs(ctx context.Context) {
	if r.gh == nil || r.maxLifetime <= 0 {
		return
	}
	jobs, err := r.store.ListJobs(ctx, store.JobListFilter{
		Statuses: []string{"in_progress"},
		Limit:    staleInProgressListLimit,
	})
	if err != nil {
		r.log.Error("reconciler/github: ListJobs(in_progress) failed", "err", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	now := r.nowFn()
	stale := make([]*store.Job, 0, len(jobs))
	for _, j := range jobs {
		if now.Sub(j.UpdatedAt) >= r.maxLifetime {
			stale = append(stale, j)
		}
	}
	if len(stale) == 0 {
		return
	}

	repos, err := r.store.ListAllInstallationRepos(ctx)
	if err != nil {
		r.log.Error("reconciler/github: ListAllInstallationRepos failed; skipping stale in_progress sweep", "err", err)
		return
	}
	repoToInstall := make(map[string]int64, len(repos))
	for _, ri := range repos {
		repoToInstall[ri.Repo] = ri.InstallationID
	}

	appCfg, err := r.store.GetAppConfig(ctx)
	if err != nil {
		r.log.Error("reconciler/github: GetAppConfig failed; skipping stale in_progress sweep", "err", err)
		return
	}
	if appCfg == nil {
		r.log.Debug("reconciler/github: app config not set; skipping stale in_progress sweep")
		return
	}
	jwt, err := r.gh.AppJWT(appCfg.PEM, appCfg.AppID)
	if err != nil {
		r.log.Error("reconciler/github: AppJWT failed; skipping stale in_progress sweep", "err", err)
		return
	}

	tokens := map[int64]string{}
	// failedInstalls remembers installations whose token mint failed
	// in this tick so subsequent stale jobs sharing the installation
	// don't each re-attempt the mint and burn quota. A flaky token
	// endpoint with N stale jobs in one installation would otherwise
	// cost N mint attempts per sweep.
	failedInstalls := map[int64]struct{}{}
	fixed := 0
	for _, j := range stale {
		instID, ok := repoToInstall[j.Repo]
		if !ok {
			r.log.Warn("reconciler/github: stale in_progress job has no installation; leaving",
				"job_id", j.ID, "repo", j.Repo, "age", now.Sub(j.UpdatedAt))
			continue
		}
		if _, failed := failedInstalls[instID]; failed {
			r.log.Debug("reconciler/github: skipping job — token mint already failed this tick",
				"job_id", j.ID, "repo", j.Repo, "installation_id", instID)
			continue
		}
		tok, ok := tokens[instID]
		if !ok {
			tok, err = r.gh.InstallationToken(ctx, jwt, instID)
			if err != nil {
				r.log.Error("reconciler/github: InstallationToken failed; skipping installation this tick",
					"job_id", j.ID, "repo", j.Repo, "installation_id", instID, "err", err)
				failedInstalls[instID] = struct{}{}
				continue
			}
			tokens[instID] = tok
		}
		live, err := r.gh.WorkflowJob(ctx, tok, j.Repo, j.ID)
		if err != nil {
			r.log.Warn("reconciler/github: WorkflowJob failed; leaving stale row",
				"job_id", j.ID, "repo", j.Repo, "err", err)
			continue
		}
		switch {
		case live.NotFound:
			// Job no longer exists on GitHub. Mirror dispatch's NotFound
			// policy: treat as cancelled.
			if _, err := r.store.MarkJobCompleted(ctx, j.ID, "cancelled"); err != nil {
				r.log.Error("reconciler/github: MarkJobCompleted(cancelled) failed",
					"job_id", j.ID, "err", err)
				continue
			}
			r.log.Info("reconciler/github: stale in_progress reconciled (NotFound)",
				"job_id", j.ID, "repo", j.Repo, "age", now.Sub(j.UpdatedAt))
			fixed++
		case live.AuthFailed:
			// AuthFailed here means the install token we just minted
			// successfully was rejected on the per-call check — most
			// commonly the App was just uninstalled. The webhook for
			// completion can no longer arrive either, so leaving the
			// row in_progress would burn a WorkflowJob call every
			// sweep cycle forever. Treat as terminal/cancelled, same
			// policy as NotFound.
			if _, err := r.store.MarkJobCompleted(ctx, j.ID, "cancelled"); err != nil {
				r.log.Error("reconciler/github: MarkJobCompleted(cancelled) after AuthFailed failed",
					"job_id", j.ID, "err", err)
				continue
			}
			r.log.Info("reconciler/github: stale in_progress reconciled (AuthFailed)",
				"job_id", j.ID, "repo", j.Repo, "age", now.Sub(j.UpdatedAt))
			fixed++
		case live.Status == "completed":
			concl := live.Conclusion
			if concl == "" {
				// GH reports completed but no conclusion — extremely
				// rare, but guard against it so we always write a
				// non-empty terminal value.
				concl = "neutral"
			}
			if _, err := r.store.MarkJobCompleted(ctx, j.ID, concl); err != nil {
				r.log.Error("reconciler/github: MarkJobCompleted failed",
					"job_id", j.ID, "err", err)
				continue
			}
			r.log.Info("reconciler/github: stale in_progress reconciled",
				"job_id", j.ID, "repo", j.Repo, "conclusion", concl, "age", now.Sub(j.UpdatedAt))
			fixed++
		default:
			// GH still says queued/in_progress: webhook isn't lost,
			// the job is genuinely still running. Leave it alone.
			r.log.Debug("reconciler/github: stale in_progress still running on GitHub; leaving",
				"job_id", j.ID, "repo", j.Repo, "gh_status", live.Status, "age", now.Sub(j.UpdatedAt))
		}
	}
	if fixed > 0 {
		// Surface actual reconciliation activity at Info so operators
		// can see it without enabling debug. The empty-tick case
		// stays at Debug — that's the steady state we don't want to
		// spam the log with.
		r.log.Info("reconciler/github: stale in_progress sweep complete",
			"candidates", len(stale), "fixed", fixed)
	} else {
		r.log.Debug("reconciler/github: stale in_progress sweep complete",
			"candidates", len(stale), "fixed", fixed)
	}
}

func (r *Reconciler) runMaintenanceSweeper(ctx context.Context) {
	t := time.NewTicker(r.maintenancePeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runMaintenanceCommand(ctx)
		}
	}
}

// runMaintenanceCommand executes maintenanceCmd as a subprocess. stdout
// and stderr are captured and logged at Info on success, Warn on
// non-zero exit. Errors are never fatal — a failing prune command
// should not crash the service.
func (r *Reconciler) runMaintenanceCommand(ctx context.Context) {
	if len(r.maintenanceCmd) == 0 {
		return
	}
	cmd := exec.CommandContext(ctx, r.maintenanceCmd[0], r.maintenanceCmd[1:]...) //nolint:gosec // user-supplied, intentional
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		r.log.Warn("reconciler: maintenance command failed",
			"cmd", r.maintenanceCmd, "err", err, "output", output)
		return
	}
	r.log.Info("reconciler: maintenance command succeeded",
		"cmd", r.maintenanceCmd, "output", output)
}
