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
// own CreatedAt is past the grace window.
//
// The grace check is per-container, not per-host: an earlier version
// deferred the entire sweep whenever any active runner row was younger
// than the grace, which broke under continuous load — a host with a
// steady stream of new dispatches would never clean up actual orphans
// because there was always a young row in the table.
func (r *Reconciler) sweepOrphanContainers(ctx context.Context, known map[string]struct{}) {
	cs, err := r.docker.ListByPrefix(ctx, ContainerNamePrefix)
	if err != nil {
		r.log.Error("reconciler: ListByPrefix failed", "err", err)
		return
	}
	if len(cs) == 0 {
		return
	}
	now := r.nowFn()
	for _, c := range cs {
		if !strings.HasPrefix(c.Name, ContainerNamePrefix) {
			continue // belt-and-suspenders; ListByPrefix already filtered
		}
		if _, ok := known[c.Name]; ok {
			continue
		}
		// Per-container grace: protect the brief window between
		// `docker run` ack and the InsertRunner row becoming visible.
		// Zero CreatedAt (parse failure) is treated as old — better to
		// remove an undatable orphan than to leak it forever.
		if !c.CreatedAt.IsZero() && now.Sub(c.CreatedAt) < r.grace {
			r.log.Debug("reconciler: deferring orphan removal during grace window", "container", c.Name, "age", now.Sub(c.CreatedAt))
			continue
		}
		if err := r.docker.ForceRemove(ctx, c.Name); err != nil {
			r.log.Error("reconciler: ForceRemove failed", "container", c.Name, "err", err)
			continue
		}
		r.log.Info("reconciler: orphan container removed", "container", c.Name)
	}
}
