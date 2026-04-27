package store

import (
	"context"
	"errors"
)

// SQLite is a placeholder. v1 implementation will use modernc.org/sqlite (CGO-free).
type SQLite struct {
	dsn string
}

func OpenSQLite(dsn string) (*SQLite, error) {
	return &SQLite{dsn: dsn}, nil
}

var errNotImplemented = errors.New("store/sqlite: not implemented yet")

func (s *SQLite) SaveAppConfig(ctx context.Context, cfg *AppConfig) error { return errNotImplemented }
func (s *SQLite) GetAppConfig(ctx context.Context) (*AppConfig, error)    { return nil, errNotImplemented }

func (s *SQLite) UpsertInstallation(ctx context.Context, inst *Installation) error {
	return errNotImplemented
}
func (s *SQLite) ListInstallations(ctx context.Context) ([]*Installation, error) {
	return nil, errNotImplemented
}
func (s *SQLite) UpsertRepoInstallation(ctx context.Context, repoFullName string, installationID int64) error {
	return errNotImplemented
}
func (s *SQLite) RemoveRepoInstallation(ctx context.Context, repoFullName string) error {
	return errNotImplemented
}
func (s *SQLite) InstallationForRepo(ctx context.Context, repoFullName string) (*Installation, error) {
	return nil, errNotImplemented
}

func (s *SQLite) InsertJobIfNew(ctx context.Context, j *Job) (bool, error) {
	return false, errNotImplemented
}
func (s *SQLite) GetJob(ctx context.Context, jobID int64) (*Job, error) {
	return nil, errNotImplemented
}
func (s *SQLite) MarkJobInProgress(ctx context.Context, jobID int64, runnerID int64, runnerName string) error {
	return errNotImplemented
}
func (s *SQLite) MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) error {
	return errNotImplemented
}
func (s *SQLite) PendingJobs(ctx context.Context) ([]*Job, error) { return nil, errNotImplemented }

func (s *SQLite) InsertRunner(ctx context.Context, r *Runner) error { return errNotImplemented }
func (s *SQLite) UpdateRunnerStatus(ctx context.Context, containerName, status string) error {
	return errNotImplemented
}
func (s *SQLite) UpdateRunnerStatusByName(ctx context.Context, runnerName, status string) error {
	return errNotImplemented
}
func (s *SQLite) ActiveRunnerCount(ctx context.Context) (int, error)        { return 0, errNotImplemented }
func (s *SQLite) ListActiveRunners(ctx context.Context) ([]*Runner, error) { return nil, errNotImplemented }

func (s *SQLite) Close() error { return nil }
