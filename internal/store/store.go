package store

import "context"

type Store interface {
	SaveAppConfig(ctx context.Context, cfg *AppConfig) error
	GetAppConfig(ctx context.Context) (*AppConfig, error)

	UpsertInstallation(ctx context.Context, inst *Installation) error
	ListInstallations(ctx context.Context) ([]*Installation, error)
	InstallationForRepo(ctx context.Context, repoFullName string) (*Installation, error)

	InsertJobIfNew(ctx context.Context, j *Job) (inserted bool, err error)
	UpdateJobStatus(ctx context.Context, jobID int64, status string, runnerID int64, runnerName string) error
	PendingJobs(ctx context.Context) ([]*Job, error)

	InsertRunner(ctx context.Context, r *Runner) error
	UpdateRunnerStatus(ctx context.Context, containerName, status string) error
	ActiveRunnerCount(ctx context.Context) (int, error)
	ListActiveRunners(ctx context.Context) ([]*Runner, error)

	Close() error
}
