package scheduler

import (
	"context"
	"log/slog"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/runner"
	"github.com/muhac/actions-runner-pool/internal/store"
)

type Scheduler struct {
	cfg    *config.Config
	store  store.Store
	gh     *github.Client
	runner *runner.Launcher
	jobCh  chan int64
	log    *slog.Logger
}

func New(cfg *config.Config, st store.Store, gh *github.Client, rn *runner.Launcher, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:    cfg,
		store:  st,
		gh:     gh,
		runner: rn,
		jobCh:  make(chan int64, 256),
		log:    log,
	}
}

// Enqueue is called from the webhook handler after the job row is persisted.
// Non-blocking: if the channel is full, the job stays pending in sqlite and
// the recovery loop will pick it up.
func (s *Scheduler) Enqueue(jobID int64) {
	select {
	case s.jobCh <- jobID:
	default:
		s.log.Warn("scheduler queue full, job remains pending in store", "job_id", jobID)
	}
}

// Run starts the worker loop. Blocks until ctx is cancelled.
// TODO: replay pending jobs from store on startup; concurrency-cap check; per-job dispatch.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case jobID := <-s.jobCh:
			s.log.Info("scheduler picked job", "job_id", jobID)
			// TODO: dispatch — load job, check concurrency, mint tokens, launch runner.
		}
	}
}
