package store

import "context"

type Store interface {
	SaveAppConfig(ctx context.Context, cfg *AppConfig) error
	GetAppConfig(ctx context.Context) (*AppConfig, error)

	UpsertInstallation(ctx context.Context, inst *Installation) error
	ListInstallations(ctx context.Context) ([]*Installation, error)

	UpsertRepoInstallation(ctx context.Context, repoFullName string, installationID int64) error
	RemoveRepoInstallation(ctx context.Context, repoFullName string) error
	InstallationForRepo(ctx context.Context, repoFullName string) (*Installation, error)

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
	// be resurrected by a stale event.
	MarkJobInProgress(ctx context.Context, jobID int64, runnerID int64, runnerName string) error
	MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) error
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
