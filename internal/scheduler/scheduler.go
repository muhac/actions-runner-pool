package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
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
}

type Scheduler struct {
	cfg    *config.Config
	store  store.Store
	gh     GitHubClient
	runner Launcher
	jobCh  chan int64
	log    *slog.Logger

	capBackoff time.Duration
	nameFn     func(jobID int64) (string, string)
}

func New(cfg *config.Config, st store.Store, gh GitHubClient, rn Launcher, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:        cfg,
		store:      st,
		gh:         gh,
		runner:     rn,
		jobCh:      make(chan int64, 256),
		log:        log,
		capBackoff: 2 * time.Second,
		nameFn:     defaultNameFn,
	}
}

// Enqueue is non-blocking: if the channel is full the job stays pending in
// sqlite and the next startup-replay picks it up.
func (s *Scheduler) Enqueue(jobID int64) {
	select {
	case s.jobCh <- jobID:
	default:
		s.log.Warn("scheduler queue full, job remains pending in store", "job_id", jobID)
	}
}

func (s *Scheduler) Run(ctx context.Context) error {
	s.replay(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case jobID := <-s.jobCh:
			s.dispatch(ctx, jobID)
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
	// Cap check must happen before any GitHub API call so we never burn
	// rate limit on a job we can't launch.
	n, err := s.store.ActiveRunnerCount(ctx)
	if err != nil {
		s.log.Error("dispatch: ActiveRunnerCount failed; leaving pending", "job_id", jobID, "err", err)
		return
	}
	if n >= s.cfg.MaxConcurrentRunners {
		s.log.Debug("dispatch: at concurrency cap, re-enqueueing", "job_id", jobID, "active", n, "cap", s.cfg.MaxConcurrentRunners)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.capBackoff):
		}
		s.Enqueue(jobID)
		return
	}

	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		s.log.Error("dispatch: GetJob failed; leaving pending", "job_id", jobID, "err", err)
		return
	}
	if job == nil {
		s.log.Warn("dispatch: job not found", "job_id", jobID)
		return
	}
	if job.Status != "pending" {
		s.log.Debug("dispatch: job no longer pending, skipping", "job_id", jobID, "status", job.Status)
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
		RepoURL:           "https://github.com/" + job.Repo,
		Labels:            rowLabels,
		Image:             s.cfg.RunnerImage,
	}); err != nil {
		s.log.Error("dispatch: Launch failed", "job_id", jobID, "container", containerName, "err", err)
		if uerr := s.store.UpdateRunnerStatus(ctx, containerName, "finished"); uerr != nil {
			s.log.Error("dispatch: UpdateRunnerStatus(finished) after launch error failed", "container", containerName, "err", uerr)
		}
		return
	}

	// Advance the job past 'pending' so a restart's PendingJobs replay
	// won't re-dispatch it. We don't yet know which runner GitHub will
	// pick (workflow_job: in_progress carries that), so use sentinel
	// runner_id=0 / runner_name="" — the real values land later.
	if err := s.store.MarkJobInProgress(ctx, jobID, 0, ""); err != nil {
		s.log.Error("dispatch: MarkJobInProgress(sentinel) after launch failed", "job_id", jobID, "err", err)
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
