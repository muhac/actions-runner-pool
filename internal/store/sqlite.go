package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type SQLite struct {
	db *sql.DB
}

func OpenSQLite(dsn string) (*SQLite, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite (%s): %w", dsn, err)
	}
	if _, err := db.ExecContext(context.Background(), schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

func (s *SQLite) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
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
func (s *SQLite) ActiveRunnerCount(ctx context.Context) (int, error)       { return 0, errNotImplemented }
func (s *SQLite) ListActiveRunners(ctx context.Context) ([]*Runner, error) { return nil, errNotImplemented }
