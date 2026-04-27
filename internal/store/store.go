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
	MarkJobInProgress(ctx context.Context, jobID int64, runnerID int64, runnerName string) error
	MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) error
	PendingJobs(ctx context.Context) ([]*Job, error)

	InsertRunner(ctx context.Context, r *Runner) error
	UpdateRunnerStatus(ctx context.Context, containerName, status string) error
	UpdateRunnerStatusByName(ctx context.Context, runnerName, status string) error
	ActiveRunnerCount(ctx context.Context) (int, error)
	ListActiveRunners(ctx context.Context) ([]*Runner, error)

	Close() error
}
