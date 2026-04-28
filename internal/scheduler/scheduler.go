package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/runner"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// Launcher is the subset of *runner.Launcher dispatch depends on. Defined
// here so tests can swap a spy without spinning up docker.
type Launcher interface {
	Launch(ctx context.Context, spec runner.Spec) error
}

// GitHubClient is the subset of *github.Client dispatch calls. Same motivation.
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

	// capBackoff is how long dispatch waits before re-enqueueing when the
	// concurrency cap is hit. Test hook.
	capBackoff time.Duration
	// nameFn produces (containerName, runnerName) for a fresh dispatch.
	// Test hook so assertions can pin a value.
	nameFn func(jobID int64) (string, string)
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

// Enqueue is called from the webhook handler after the job row is persisted,
// and from dispatch itself when the concurrency cap re-queues a job.
// Non-blocking: if the channel is full the job stays pending in sqlite and
// the next startup-replay (or a later Enqueue with capacity) picks it up.
func (s *Scheduler) Enqueue(jobID int64) {
	select {
	case s.jobCh <- jobID:
	default:
		s.log.Warn("scheduler queue full, job remains pending in store", "job_id", jobID)
	}
}

// Run replays pending jobs on startup, then drains the channel, dispatching
// each job. Blocks until ctx is cancelled.
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

// replay re-enqueues every pending job from the store. Channel buffer is
// finite — overflow is logged via Enqueue's default branch and stays pending
// until the next startup or a later capacity opening.
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

// dispatch processes one jobID. Sequencing matters — see comments inline.
func (s *Scheduler) dispatch(ctx context.Context, jobID int64) {
	// 1. Concurrency cap BEFORE any token mint or API call. If we're over,
	//    re-enqueue (with a small backoff to avoid a busy spin) and bail.
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

	// 2. Load the job. Skip if it's already been advanced (in_progress /
	//    completed) — typical when the channel and store disagree across a
	//    restart, or after a duplicate Enqueue.
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

	// 3. Map repo -> installation. A missing row means the installation
	//    webhook hasn't landed yet (or was missed); leave the job pending so
	//    the next replay picks it up after the lazy-write in webhook.
	inst, err := s.store.InstallationForRepo(ctx, job.Repo)
	if err != nil {
		s.log.Error("dispatch: InstallationForRepo failed", "job_id", jobID, "repo", job.Repo, "err", err)
		return
	}
	if inst == nil {
		s.log.Warn("dispatch: no installation for repo; leaving pending", "job_id", jobID, "repo", job.Repo)
		return
	}

	// 4. App credentials.
	appCfg, err := s.store.GetAppConfig(ctx)
	if err != nil || appCfg == nil {
		s.log.Error("dispatch: GetAppConfig failed; leaving pending", "job_id", jobID, "err", err)
		return
	}

	// 5. Mint JWT -> installation token -> registration token.
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

	// 6. Insert runner row in 'starting' before the launch — if we crash
	//    between Insert and Launch, reconciliation (v1.1) sees a starting
	//    runner with no live container and can clean up.
	containerName, runnerName := s.nameFn(jobID)
	rowLabels := strings.Join(s.cfg.RunnerLabels, ",")
	if rowLabels == "" {
		rowLabels = job.Labels
	}
	if err := s.store.InsertRunner(ctx, &store.Runner{
		ContainerName: containerName,
		Repo:          job.Repo,
		RunnerName:    runnerName,
		Labels:        rowLabels,
		Status:        "starting",
	}); err != nil {
		s.log.Error("dispatch: InsertRunner failed", "job_id", jobID, "err", err)
		return
	}

	// 7. Launch.
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
	s.log.Info("dispatch: runner launched", "job_id", jobID, "container", containerName, "runner_name", runnerName)
}

// defaultNameFn returns ("gharp-<jobID>-<8 hex>", same value).
// container_name and runner_name share the value — both columns must be
// populated from v1 (per architecture.md), and no caller depends on them
// being distinct.
func defaultNameFn(jobID int64) (string, string) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure on a sane system means the world is on fire;
		// fall back to a timestamp suffix so dispatch can still proceed.
		name := fmt.Sprintf("gharp-%d-%d", jobID, time.Now().UnixNano())
		return name, name
	}
	name := fmt.Sprintf("gharp-%d-%s", jobID, hex.EncodeToString(b[:]))
	return name, name
}
