package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type SQLite struct {
	db *sql.DB
}

func OpenSQLite(dsn string) (*SQLite, error) {
	return OpenSQLiteWithContext(context.Background(), dsn)
}

func OpenSQLiteWithContext(ctx context.Context, dsn string) (*SQLite, error) {
	// foreign_keys is per-connection in sqlite; setting it via the DSN ensures
	// every connection in database/sql's pool gets it (a one-shot
	// `PRAGMA foreign_keys = ON` would only stick on a single connection).
	dsn = ensureDSNPragma(dsn, "foreign_keys", "1")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite (%s): %w", dsn, err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &SQLite{db: db}, nil
}

// ensureDSNPragma adds `_pragma=name(value)` to a modernc.org/sqlite DSN if
// the named pragma is not already present.
func ensureDSNPragma(dsn, name, value string) string {
	q := name + "("
	// Check if already set in any _pragma= clause.
	if strings.Contains(dsn, "_pragma="+q) || strings.Contains(dsn, "_pragma="+url.QueryEscape(q)) {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=" + url.QueryEscape(name+"("+value+")")
}

func (s *SQLite) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ---------------- app_config ----------------

func (s *SQLite) SaveAppConfig(ctx context.Context, cfg *AppConfig) error {
	const q = `
INSERT INTO app_config (id, app_id, slug, webhook_secret, pem, client_id, client_secret, base_url)
VALUES (1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  app_id=excluded.app_id, slug=excluded.slug, webhook_secret=excluded.webhook_secret,
  pem=excluded.pem, client_id=excluded.client_id, client_secret=excluded.client_secret,
  base_url=excluded.base_url`
	_, err := s.db.ExecContext(ctx, q,
		cfg.AppID, cfg.Slug, cfg.WebhookSecret, cfg.PEM,
		cfg.ClientID, cfg.ClientSecret, cfg.BaseURL)
	return err
}

func (s *SQLite) GetAppConfig(ctx context.Context) (*AppConfig, error) {
	const q = `SELECT app_id, slug, webhook_secret, pem, client_id, client_secret, base_url, created_at
		FROM app_config WHERE id = 1`
	var c AppConfig
	err := s.db.QueryRowContext(ctx, q).Scan(
		&c.AppID, &c.Slug, &c.WebhookSecret, &c.PEM,
		&c.ClientID, &c.ClientSecret, &c.BaseURL, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ---------------- installations ----------------

func (s *SQLite) UpsertInstallation(ctx context.Context, inst *Installation) error {
	const q = `
INSERT INTO installations (id, account_id, account_login, account_type)
VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  account_id=excluded.account_id, account_login=excluded.account_login,
  account_type=excluded.account_type`
	_, err := s.db.ExecContext(ctx, q,
		inst.ID, inst.AccountID, inst.AccountLogin, inst.AccountType)
	return err
}

func (s *SQLite) ListInstallations(ctx context.Context) ([]*Installation, error) {
	const q = `SELECT id, account_id, account_login, account_type, created_at
		FROM installations ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Installation
	for rows.Next() {
		var i Installation
		if err := rows.Scan(&i.ID, &i.AccountID, &i.AccountLogin, &i.AccountType, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &i)
	}
	return out, rows.Err()
}

func (s *SQLite) UpsertRepoInstallation(ctx context.Context, repoFullName string, installationID int64) error {
	const q = `
INSERT INTO installation_repos (repo, installation_id) VALUES (?, ?)
ON CONFLICT(repo) DO UPDATE SET installation_id = excluded.installation_id`
	_, err := s.db.ExecContext(ctx, q, repoFullName, installationID)
	return err
}

func (s *SQLite) RemoveRepoInstallation(ctx context.Context, repoFullName string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM installation_repos WHERE repo = ?`, repoFullName)
	return err
}

func (s *SQLite) InstallationForRepo(ctx context.Context, repoFullName string) (*Installation, error) {
	const q = `
SELECT i.id, i.account_id, i.account_login, i.account_type, i.created_at
FROM installation_repos r
JOIN installations i ON i.id = r.installation_id
WHERE r.repo = ?`
	var i Installation
	err := s.db.QueryRowContext(ctx, q, repoFullName).Scan(
		&i.ID, &i.AccountID, &i.AccountLogin, &i.AccountType, &i.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// ---------------- jobs ----------------

func (s *SQLite) InsertJobIfNew(ctx context.Context, j *Job) (bool, error) {
	const q = `
INSERT INTO jobs (id, repo, action, labels, dedupe_key, status, conclusion, runner_id, runner_name)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(dedupe_key) DO NOTHING`
	res, err := s.db.ExecContext(ctx, q,
		j.ID, j.Repo, j.Action, j.Labels, j.DedupeKey, j.Status, j.Conclusion, j.RunnerID, j.RunnerName)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *SQLite) GetJob(ctx context.Context, jobID int64) (*Job, error) {
	const q = `SELECT id, repo, action, labels, dedupe_key, status, conclusion,
		runner_id, runner_name, received_at, updated_at FROM jobs WHERE id = ?`
	var j Job
	err := s.db.QueryRowContext(ctx, q, jobID).Scan(
		&j.ID, &j.Repo, &j.Action, &j.Labels, &j.DedupeKey, &j.Status, &j.Conclusion,
		&j.RunnerID, &j.RunnerName, &j.ReceivedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *SQLite) MarkJobInProgress(ctx context.Context, jobID int64, runnerID int64, runnerName string) error {
	const q = `UPDATE jobs SET status='in_progress', runner_id=?, runner_name=?,
		updated_at=CURRENT_TIMESTAMP WHERE id=?`
	_, err := s.db.ExecContext(ctx, q, runnerID, runnerName, jobID)
	return err
}

func (s *SQLite) MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) error {
	const q = `UPDATE jobs SET status='completed', conclusion=?,
		updated_at=CURRENT_TIMESTAMP WHERE id=?`
	_, err := s.db.ExecContext(ctx, q, conclusion, jobID)
	return err
}

func (s *SQLite) PendingJobs(ctx context.Context) ([]*Job, error) {
	const q = `SELECT id, repo, action, labels, dedupe_key, status, conclusion,
		runner_id, runner_name, received_at, updated_at
		FROM jobs WHERE status='pending' ORDER BY received_at, id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Repo, &j.Action, &j.Labels, &j.DedupeKey, &j.Status, &j.Conclusion,
			&j.RunnerID, &j.RunnerName, &j.ReceivedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &j)
	}
	return out, rows.Err()
}

// ---------------- runners ----------------

func (s *SQLite) InsertRunner(ctx context.Context, r *Runner) error {
	const q = `INSERT INTO runners (container_name, repo, runner_name, labels, status, started_at)
		VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q,
		r.ContainerName, r.Repo, r.RunnerName, r.Labels, r.Status, r.StartedAt)
	return err
}

func (s *SQLite) UpdateRunnerStatus(ctx context.Context, containerName, status string) error {
	return s.updateRunnerStatus(ctx, "container_name", containerName, status)
}

func (s *SQLite) UpdateRunnerStatusByName(ctx context.Context, runnerName, status string) error {
	return s.updateRunnerStatus(ctx, "runner_name", runnerName, status)
}

func (s *SQLite) updateRunnerStatus(ctx context.Context, col, val, status string) error {
	// col is one of two trusted constants from the methods above; safe to interpolate.
	q := `UPDATE runners SET status=?,
		finished_at = CASE WHEN ?='finished' THEN CURRENT_TIMESTAMP ELSE finished_at END
		WHERE ` + col + ` = ?`
	_, err := s.db.ExecContext(ctx, q, status, status, val)
	return err
}

func (s *SQLite) ActiveRunnerCount(ctx context.Context) (int, error) {
	const q = `SELECT count(*) FROM runners WHERE status IN ('starting','idle','busy')`
	var n int
	err := s.db.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

func (s *SQLite) ListActiveRunners(ctx context.Context) ([]*Runner, error) {
	const q = `SELECT container_name, repo, runner_name, labels, status, started_at, finished_at
		FROM runners WHERE status IN ('starting','idle','busy') ORDER BY started_at, container_name`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Runner
	for rows.Next() {
		var r Runner
		var fin sql.NullTime
		if err := rows.Scan(&r.ContainerName, &r.Repo, &r.RunnerName, &r.Labels,
			&r.Status, &r.StartedAt, &fin); err != nil {
			return nil, err
		}
		if fin.Valid {
			t := fin.Time
			r.FinishedAt = &t
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
