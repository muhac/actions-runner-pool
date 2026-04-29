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
// Out of scope for this pass: GitHub-side ghost runner deregistration,
// idle-timeout reaping, dispatch-time live cap checks. See
// docs/architecture.md §"Ghost runners and reconciliation".
package reconciler

import (
	"context"
	"log/slog"
	"strings"
	"time"

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
}

type Reconciler struct {
	store               Store
	docker              Docker
	log                 *slog.Logger
	period              time.Duration
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
// DefaultContainerNamePrefix.
func New(st Store, dk Docker, log *slog.Logger, maxLifetime time.Duration, containerNamePrefix string) *Reconciler {
	if containerNamePrefix == "" {
		containerNamePrefix = DefaultContainerNamePrefix
	}
	return &Reconciler{
		store:  st,
		docker: dk,
		log:    log,
		period: 60 * time.Second,
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

// Run blocks until ctx is cancelled. Ticks every period; each tick is
// a single Reconcile call. Reconcile errors are logged, never
// propagated — the loop must keep running across transient docker
// hiccups.
func (r *Reconciler) Run(ctx context.Context) error {
	r.Reconcile(ctx)
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
