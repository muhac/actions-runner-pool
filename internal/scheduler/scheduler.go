package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/runner"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// Launcher is the subset of *runner.Launcher dispatch depends on.
type Launcher interface {
	Launch(ctx context.Context, spec runner.Spec) error
}

// GitHubClient is the subset of *github.Client dispatch calls.
type GitHubClient interface {
	AppJWT(pem []byte, appID int64) (string, error)
	InstallationToken(ctx context.Context, jwt string, installationID int64) (string, error)
	RegistrationToken(ctx context.Context, installationToken, repoFullName string) (string, error)
	// WorkflowJob fetches the live job state from GitHub. Used as the
	// pre-launch correctness check that catches jobs which
	// completed/cancelled while we were minting tokens (or whose
	// state-change webhooks were delayed/dropped).
	WorkflowJob(ctx context.Context, installationToken, repoFullName string, jobID int64) (*github.WorkflowJobStatus, error)
}

type Scheduler struct {
	cfg    *config.Config
	store  store.Store
	gh     GitHubClient
	runner Launcher
	jobCh  chan int64
	log    *slog.Logger

	capBackoff    time.Duration
	replayPeriod  time.Duration
	nameFn        func(jobID int64) (string, string)
}

func New(cfg *config.Config, st store.Store, gh GitHubClient, rn Launcher, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:          cfg,
		store:        st,
		gh:           gh,
		runner:       rn,
		jobCh:        make(chan int64, 256),
		log:          log,
		capBackoff:   2 * time.Second,
		replayPeriod: 1 * time.Minute,
		nameFn:       defaultNameFn,
	}
}

// Enqueue is non-blocking: if the channel is full the job stays pending in
// sqlite and the next replay picks it up.
func (s *Scheduler) Enqueue(jobID int64) {
	select {
	case s.jobCh <- jobID:
	default:
		s.log.Warn("scheduler queue full, job remains pending in store", "job_id", jobID)
	}
}

func (s *Scheduler) Run(ctx context.Context) error {
	s.replay(ctx)
	ticker := time.NewTicker(s.replayPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case jobID := <-s.jobCh:
			s.dispatch(ctx, jobID)
		case <-ticker.C:
			// Periodic rescue: any 'dispatched' row whose runner never
			// claimed a job (the runner↔job race documented in
			// architecture.md) becomes eligible after dispatchedReplayAge
			// and gets re-dispatched here. Cheap when nothing is stuck —
			// PendingJobs returns an empty slice in the steady state.
			s.replay(ctx)
		}
	}
}

func (s *Scheduler) replay(ctx context.Context) {
	jobs, err := s.store.PendingJobs(ctx)
	if err != nil {
		s.log.Error("scheduler replay: PendingJobs failed", "err", err)
		return
	}
	if len(jobs) == 0 {
		return
	}
	s.log.Info("scheduler replay", "pending", len(jobs))
	for _, j := range jobs {
		s.Enqueue(j.ID)
	}
}

func (s *Scheduler) dispatch(ctx context.Context, jobID int64) {
	// Load the job FIRST so non-pending jobs short-circuit immediately —
	// otherwise we'd keep AfterFunc-re-enqueueing them forever while at
	// cap. The cap-before-GitHub-API invariant still holds: GetJob is
	// store-only, no GitHub call yet.
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		s.log.Error("dispatch: GetJob failed; leaving pending", "job_id", jobID, "err", err)
		return
	}
	if job == nil {
		s.log.Warn("dispatch: job not found", "job_id", jobID)
		return
	}
	if job.Status != "pending" && job.Status != "dispatched" {
		s.log.Debug("dispatch: job no longer rescuable, skipping", "job_id", jobID, "status", job.Status)
		return
	}

	// Cap check before any GitHub API call so we never burn rate limit
	// on a job we can't launch.
	n, err := s.store.ActiveRunnerCount(ctx)
	if err != nil {
		s.log.Error("dispatch: ActiveRunnerCount failed; leaving pending", "job_id", jobID, "err", err)
		return
	}
	if n >= s.cfg.MaxConcurrentRunners {
		s.log.Debug("dispatch: at concurrency cap, scheduling re-enqueue", "job_id", jobID, "active", n, "cap", s.cfg.MaxConcurrentRunners, "backoff", s.capBackoff)
		// Re-enqueue asynchronously so the worker loop keeps draining
		// other jobs (and observing ctx.Done) instead of parking on a
		// sleep here. AfterFunc fires on its own goroutine; if ctx is
		// already cancelled by the time it runs, drop the re-enqueue.
		time.AfterFunc(s.capBackoff, func() {
			if ctx.Err() != nil {
				return
			}
			s.Enqueue(jobID)
		})
		return
	}

	inst, err := s.store.InstallationForRepo(ctx, job.Repo)
	if err != nil {
		s.log.Error("dispatch: InstallationForRepo failed", "job_id", jobID, "repo", job.Repo, "err", err)
		return
	}
	if inst == nil {
		s.log.Warn("dispatch: no installation for repo; leaving pending", "job_id", jobID, "repo", job.Repo)
		return
	}

	appCfg, err := s.store.GetAppConfig(ctx)
	if err != nil || appCfg == nil {
		s.log.Error("dispatch: GetAppConfig failed; leaving pending", "job_id", jobID, "err", err)
		return
	}

	jwtStr, err := s.gh.AppJWT(appCfg.PEM, appCfg.AppID)
	if err != nil {
		s.log.Error("dispatch: AppJWT failed", "job_id", jobID, "err", err)
		return
	}
	instTok, err := s.gh.InstallationToken(ctx, jwtStr, inst.ID)
	if err != nil {
		s.log.Error("dispatch: InstallationToken failed", "job_id", jobID, "err", err)
		return
	}
	regTok, err := s.gh.RegistrationToken(ctx, instTok, job.Repo)
	if err != nil {
		s.log.Error("dispatch: RegistrationToken failed", "job_id", jobID, "repo", job.Repo, "err", err)
		return
	}

	// Pre-launch truth-of-record check against GitHub. The opening
	// store.GetJob only saw what webhooks had written; the InstallationToken
	// + RegistrationToken round-trips above can take seconds, during which
	// the user may have cancelled the workflow or another runner may have
	// claimed the job. Without this we'd happily launch a container nobody
	// is ever going to bind to — straight ghost runner that pins a cap slot
	// until the reconciler clears it.
	live, err := s.gh.WorkflowJob(ctx, instTok, job.Repo, jobID)
	switch {
	case err != nil:
		// API hiccup: stay conservative and proceed. The 60s reconciler +
		// dispatchedReplayAge will catch a wasted launch; refusing on
		// transient GitHub errors would create a worse failure mode where
		// real jobs never dispatch.
		s.log.Warn("dispatch: WorkflowJob check failed; proceeding optimistically", "job_id", jobID, "err", err)
	case live.NotFound:
		// Job was deleted/inaccessible. Mark cancelled so the row stops
		// participating in replay; conclusion="cancelled" matches the
		// shape of a webhook-driven cancellation.
		s.log.Info("dispatch: GitHub returned 404 for job; marking cancelled", "job_id", jobID, "repo", job.Repo)
		if err := s.store.MarkJobCompleted(ctx, jobID, "cancelled"); err != nil {
			s.log.Error("dispatch: MarkJobCompleted(cancelled) after 404 failed", "job_id", jobID, "err", err)
		}
		return
	case live.Status != "queued":
		// Most common case: the job is already in_progress (another runner
		// won) or completed (cancelled / finished). Either way the launch
		// is wasted work — abort before InsertRunner so we don't even
		// create a row to clean up.
		s.log.Info("dispatch: GitHub reports job no longer queued; aborting launch",
			"job_id", jobID, "github_status", live.Status, "github_conclusion", live.Conclusion)
		return
	}

	// Insert before Launch so a crash in between leaves a 'starting' row
	// for v1.1 reconciliation to clean up.
	//
	// Runner labels come from the job, not cfg.RunnerLabels: GitHub matches
	// queued jobs to runners by intersecting `runs-on` with the runner's
	// registered labels, so registering with the job's full label set is
	// what makes the binding happen. cfg.RunnerLabels is admission filter
	// only (applied at the webhook).
	containerName, runnerName := s.nameFn(jobID)
	rowLabels := job.Labels
	if err := s.store.InsertRunner(ctx, &store.Runner{
		ContainerName: containerName,
		Repo:          job.Repo,
		RunnerName:    runnerName,
		Labels:        rowLabels,
		Status:        "starting",
		StartedAt:     time.Now(),
	}); err != nil {
		s.log.Error("dispatch: InsertRunner failed", "job_id", jobID, "err", err)
		return
	}

	if err := s.runner.Launch(ctx, runner.Spec{
		ContainerName:     containerName,
		RegistrationToken: regTok,
		RunnerName:        runnerName,
		RepoURL:           s.cfg.GitHubWebBase + "/" + job.Repo,
		Labels:            rowLabels,
		Image:             s.cfg.RunnerImage,
	}); err != nil {
		s.log.Error("dispatch: Launch failed", "job_id", jobID, "container", containerName, "err", err)
		if uerr := s.store.UpdateRunnerStatus(ctx, containerName, "finished"); uerr != nil {
			s.log.Error("dispatch: UpdateRunnerStatus(finished) after launch error failed", "container", containerName, "err", uerr)
		}
		return
	}

	// Advance the job out of 'pending' so a restart's PendingJobs replay
	// won't re-dispatch it. Conditional on status='pending' so we can't
	// race with the webhook's `workflow_job: in_progress` (which would
	// already have written the real runner binding).
	if err := s.store.MarkJobDispatched(ctx, jobID); err != nil {
		s.log.Error("dispatch: MarkJobDispatched after launch failed", "job_id", jobID, "err", err)
	}
	s.log.Info("dispatch: runner launched", "job_id", jobID, "container", containerName, "runner_name", runnerName)
}

func defaultNameFn(jobID int64) (string, string) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		name := fmt.Sprintf("gharp-%d-%d", jobID, time.Now().UnixNano())
		return name, name
	}
	name := fmt.Sprintf("gharp-%d-%s", jobID, hex.EncodeToString(b[:]))
	return name, name
}
