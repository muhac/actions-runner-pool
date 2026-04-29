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
//  2. Orphan container: a container whose name matches our prefix is
//     running, but no active runner row claims it. Force-remove it.
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
// Out of scope for this pass: idle-timeout reaping, dispatch-time
// live cap checks. See docs/architecture.md §"Ghost runners and
// reconciliation".
package reconciler

import (
	"context"
	"log/slog"
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
}

// GitHubClient is the subset of *github.Client the GitHub-side ghost
// sweep depends on. nil disables that sweep entirely (the docker-side
// sweeps still run on the normal cadence).
type GitHubClient interface {
	AppJWT(pem []byte, appID int64) (string, error)
	InstallationToken(ctx context.Context, jwt string, installationID int64) (string, error)
	ListRepoRunners(ctx context.Context, installationToken, repoFullName string) ([]github.RepoRunner, error)
	DeleteRepoRunner(ctx context.Context, installationToken, repoFullName string, runnerID int64) error
}

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
	nowFn               func() time.Time
}

// New constructs a Reconciler. maxLifetime is the hard upper bound on
// how long a runner row can sit in the active set before the loop
// force-removes the container and marks the row finished — defends
// against EPHEMERAL runners that registered but never claimed a job
// (no in_progress webhook ever arrives, the cap slot is held forever
// otherwise). containerNamePrefix scopes the orphan sweep so it only
// touches containers this deployment owns; pass "" to fall back to
// DefaultContainerNamePrefix. gh is optional — pass nil to skip the
// GitHub-side ghost-runner sweep entirely (useful for tests, or
// deployments that prefer to let GitHub auto-expire stale
// registrations on its own).
func New(st Store, dk Docker, gh GitHubClient, log *slog.Logger, maxLifetime time.Duration, containerNamePrefix string) *Reconciler {
	if containerNamePrefix == "" {
		containerNamePrefix = DefaultContainerNamePrefix
	}
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
		nowFn:               time.Now,
	}
}

// Run blocks until ctx is cancelled. Two tickers run concurrently:
// `period` (60s) drives the docker-side sweeps; `githubSweepPeriod`
// (5min) drives the slower GitHub-side sweep. The slower cadence
// keeps GitHub API rate-limit usage bounded — most ghost rows get
// cleared by the docker sweep on the normal cadence anyway. Errors
// are logged, never propagated.
func (r *Reconciler) Run(ctx context.Context) error {
	r.Reconcile(ctx)
	if r.gh != nil {
		r.sweepGitHubGhostRunners(ctx)
	}
	t := time.NewTicker(r.period)
	defer t.Stop()
	var ghTick <-chan time.Time
	if r.gh != nil {
		ghT := time.NewTicker(r.githubSweepPeriod)
		defer ghT.Stop()
		ghTick = ghT.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.Reconcile(ctx)
		case <-ghTick:
			r.sweepGitHubGhostRunners(ctx)
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
		r.log.Info("reconciler: orphan container removed", "container", c.Name)
		removed++
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
	if err != nil || appCfg == nil {
		r.log.Error("reconciler/github: GetAppConfig failed; skipping sweep", "err", err)
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
