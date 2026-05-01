package store

import "context"

// Store defines the interface for persisting job, runner, and app configuration.
type Store interface {
	SaveAppConfig(ctx context.Context, cfg *AppConfig) error
	GetAppConfig(ctx context.Context) (*AppConfig, error)

	UpsertInstallation(ctx context.Context, inst *Installation) error
	ListInstallations(ctx context.Context) ([]*Installation, error)

	UpsertRepoInstallation(ctx context.Context, repoFullName string, installationID int64) error
	RemoveRepoInstallation(ctx context.Context, repoFullName string) error
	InstallationForRepo(ctx context.Context, repoFullName string) (*Installation, error)
	// ListAllInstallationRepos returns every (repo, installation) the
	// App is installed on. Used by the GitHub-side ghost sweep to
	// enumerate repos that may have stale runner registrations even
	// when the local DB has no active runner row for them.
	ListAllInstallationRepos(ctx context.Context) ([]RepoInstallation, error)

	InsertJobIfNew(ctx context.Context, j *Job) (inserted bool, err error)
	GetJob(ctx context.Context, jobID int64) (*Job, error)
	// MarkJobDispatched moves a job from 'pending' to 'dispatched' — a
	// distinct intermediate state meaning "we've launched a runner but
	// don't yet know if GitHub will assign this job to it." The webhook
	// promotes 'dispatched' → 'in_progress' once GitHub binds a real
	// runner. Conditional on status='pending' so it cannot overwrite a
	// real binding written by a concurrent in_progress webhook.
	MarkJobDispatched(ctx context.Context, jobID int64) error
	// MarkJobInProgress writes the real runner binding from a
	// `workflow_job: in_progress` webhook. Only advances rows in
	// 'pending' or 'dispatched' so that completed/cancelled rows can't
	// be resurrected by a stale event. Returns whether a row was
	// actually updated; callers use this to skip side effects (like
	// flipping a finished runner back to busy) on no-op updates.
	MarkJobInProgress(ctx context.Context, jobID int64, runnerID int64, runnerName string) (advanced bool, err error)
	// MarkJobCompleted records the terminal conclusion for an admitted
	// job. Returns whether a row existed and was updated; callers use
	// this to skip runner side effects for lifecycle events belonging
	// to jobs this process never admitted.
	MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) (completed bool, err error)
	// CancelJobIfPending transitions a single job to
	// completed/cancelled but ONLY if its current status is still
	// 'pending' or 'dispatched'. Used by dispatch's defensive cancel
	// paths (no-installation, GitHub 404-confirmed) where a real
	// workflow_job: completed webhook may have already written the
	// true conclusion concurrently — we must not overwrite it.
	// Returns whether a row was actually transitioned.
	CancelJobIfPending(ctx context.Context, jobID int64) (cancelled bool, err error)
	// CancelPendingJobsForRepo marks every pending/dispatched job for
	// the repo as completed/cancelled. Used when an installation is
	// removed (App uninstalled, repo unselected) so dispatch stops
	// looping on jobs whose installation token can no longer be minted.
	// Returns the number of rows actually transitioned.
	CancelPendingJobsForRepo(ctx context.Context, repoFullName string) (int64, error)
	// RetryJobIfCompleted transitions a completed job back to pending,
	// clearing terminal conclusion + runner binding. Returns whether a
	// row was actually retried.
	RetryJobIfCompleted(ctx context.Context, jobID int64) (retried bool, err error)
	ListJobs(ctx context.Context, f JobListFilter) ([]*Job, error)
	Summary(ctx context.Context) (*Summary, error)
	// PendingJobs returns rows still owed dispatch work — both 'pending'
	// rows and stale 'dispatched' rows whose runner never claimed a job
	// (the runner↔job race documented in architecture.md).
	PendingJobs(ctx context.Context) ([]*Job, error)

	InsertRunner(ctx context.Context, r *Runner) error
	UpdateRunnerStatus(ctx context.Context, containerName, status string) error
	UpdateRunnerStatusByName(ctx context.Context, runnerName, status string) error
	ActiveRunnerCount(ctx context.Context) (int, error)
	ListActiveRunners(ctx context.Context) ([]*Runner, error)

	Close() error
}
