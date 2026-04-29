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
//     A grace window protects containers that were just started and
//     haven't been recorded yet (the InsertRunner happens BEFORE
//     `docker run`, so this race is small but possible if start fails
//     between the insert and the docker daemon ack).
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

// ContainerNamePrefix gates which containers the orphan sweep is
// allowed to remove. Must match the name format used by
// scheduler.defaultNameFn ("gharp-<jobID>-<rand>"). Anything not
// starting with this prefix is left alone — defense against accidentally
// nuking unrelated containers if someone reuses the docker socket.
const ContainerNamePrefix = "gharp-"

// Docker is the subset of docker CLI behavior the reconciler depends
// on. Kept as an interface so tests can fake it without spawning real
// containers.
type Docker interface {
	// Inspect returns whether a container with the given name currently
	// exists (any state). The reconciler only cares about
	// existence/non-existence; status detail is a later concern.
	Inspect(ctx context.Context, containerName string) (exists bool, err error)
	// ListByPrefix returns the names of containers whose name begins
	// with the given prefix, regardless of state.
	ListByPrefix(ctx context.Context, prefix string) ([]string, error)
	// ForceRemove issues `docker rm -f` on the given container name. A
	// "no such container" outcome is treated as success.
	ForceRemove(ctx context.Context, containerName string) error
}

// Store is the subset of store.Store the reconciler needs. Defined here
// so tests can supply a thin fake without implementing every method.
type Store interface {
	ListActiveRunners(ctx context.Context) ([]*store.Runner, error)
	UpdateRunnerStatus(ctx context.Context, containerName, status string) error
}

type Reconciler struct {
	store    Store
	docker   Docker
	log      *slog.Logger
	period   time.Duration
	grace    time.Duration
	nowFn    func() time.Time
}

func New(st Store, dk Docker, log *slog.Logger) *Reconciler {
	return &Reconciler{
		store:  st,
		docker: dk,
		log:    log,
		period: 60 * time.Second,
		// Orphan grace: a container can briefly exist before its runner
		// row is committed (InsertRunner is BEFORE docker run, but the
		// docker daemon may surface the container in `ps` slightly
		// before the InsertRunner txn becomes visible to a separate
		// reader connection). 30s comfortably covers that.
		grace: 30 * time.Second,
		nowFn: time.Now,
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
// after a crash recovery).
func (r *Reconciler) Reconcile(ctx context.Context) {
	known := r.sweepGhostRunners(ctx)
	r.sweepOrphanContainers(ctx, known)
}

// sweepGhostRunners walks ListActiveRunners and marks any row 'finished'
// whose container `docker inspect` cannot find. Returns the set of
// container names that ARE still alive — passed to the orphan sweep so
// it doesn't have to re-inspect them.
func (r *Reconciler) sweepGhostRunners(ctx context.Context) map[string]struct{} {
	alive := map[string]struct{}{}
	rows, err := r.store.ListActiveRunners(ctx)
	if err != nil {
		r.log.Error("reconciler: ListActiveRunners failed", "err", err)
		return alive
	}
	for _, row := range rows {
		exists, err := r.docker.Inspect(ctx, row.ContainerName)
		if err != nil {
			// Conservative: a flaky docker daemon shouldn't cause us to
			// mark live runners finished. Skip and retry next tick.
			r.log.Warn("reconciler: docker inspect failed; leaving row alone", "container", row.ContainerName, "err", err)
			alive[row.ContainerName] = struct{}{}
			continue
		}
		if exists {
			alive[row.ContainerName] = struct{}{}
			continue
		}
		// Container vanished. Free the cap slot. The dispatched job
		// (if any) is left for scheduler.replay to rescue after
		// dispatchedReplayAge.
		if err := r.store.UpdateRunnerStatus(ctx, row.ContainerName, "finished"); err != nil {
			r.log.Error("reconciler: UpdateRunnerStatus(finished) failed", "container", row.ContainerName, "err", err)
			continue
		}
		r.log.Info("reconciler: ghost runner cleared", "container", row.ContainerName, "runner_name", row.RunnerName, "repo", row.Repo)
	}
	return alive
}

// sweepOrphanContainers force-removes containers matching our name
// prefix that are not represented by an active runner row, AND whose
// inferred age is past the grace window.
//
// We can't ask docker for a container's start time without a heavier
// inspect call, so we approximate "young enough to skip" by reading the
// most-recent active runner's StartedAt: if any active row was created
// inside the grace window, we conservatively defer the orphan sweep on
// the assumption that this orphan might be the half-recorded twin of
// that runner. This is intentionally crude — false negatives mean an
// orphan survives one extra tick, which is fine.
func (r *Reconciler) sweepOrphanContainers(ctx context.Context, known map[string]struct{}) {
	names, err := r.docker.ListByPrefix(ctx, ContainerNamePrefix)
	if err != nil {
		r.log.Error("reconciler: ListByPrefix failed", "err", err)
		return
	}
	if len(names) == 0 {
		return
	}
	now := r.nowFn()
	// Gather active runners again only if needed for the grace check.
	// `known` already gives us name set; for age we need StartedAt.
	rows, err := r.store.ListActiveRunners(ctx)
	if err != nil {
		r.log.Error("reconciler: ListActiveRunners (orphan grace) failed", "err", err)
		return
	}
	youngActive := false
	for _, row := range rows {
		if now.Sub(row.StartedAt) < r.grace {
			youngActive = true
			break
		}
	}
	for _, name := range names {
		if !strings.HasPrefix(name, ContainerNamePrefix) {
			continue // belt-and-suspenders; ListByPrefix already filtered
		}
		if _, ok := known[name]; ok {
			continue
		}
		if youngActive {
			r.log.Debug("reconciler: deferring orphan removal during grace window", "container", name)
			continue
		}
		if err := r.docker.ForceRemove(ctx, name); err != nil {
			r.log.Error("reconciler: ForceRemove failed", "container", name, "err", err)
			continue
		}
		r.log.Info("reconciler: orphan container removed", "container", name)
	}
}
