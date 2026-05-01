// Package store provides SQLite-backed persistence for runner, job, and app configuration state.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// SQLite provides a SQLite-backed implementation of the Store interface.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens a SQLite database at the given DSN and initializes the schema.
func OpenSQLite(dsn string) (*SQLite, error) {
	return OpenSQLiteWithContext(context.Background(), dsn)
}

// OpenSQLiteWithContext opens a SQLite database with a context timeout for initialization.
func OpenSQLiteWithContext(ctx context.Context, dsn string) (*SQLite, error) {
	// foreign_keys is per-connection in sqlite; setting it via the DSN ensures
	// every connection in database/sql's pool gets it (a one-shot
	// `PRAGMA foreign_keys = ON` would only stick on a single connection).
	dsn = ensureDSNPragma(dsn, "foreign_keys", "1")
	// busy_timeout makes sqlite wait (rather than immediately returning
	// SQLITE_BUSY) when another writer holds the lock. 5s is generous —
	// concurrent webhook bookkeeping was hitting this in practice.
	dsn = ensureDSNPragma(dsn, "busy_timeout", "5000")
	// WAL improves read/write concurrency; if the user already set it via
	// STORE_DSN this is a no-op.
	dsn = ensureDSNPragma(dsn, "journal_mode", "WAL")
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

// ListAllInstallationRepos returns every (repo, installation) the App
// is installed on. Used by the GitHub-side ghost sweep so it can
// query repos that currently have no active runner row — without
// this, ghosts left behind after a deployment goes idle would never
// be cleared until GitHub auto-expires them.
func (s *SQLite) ListAllInstallationRepos(ctx context.Context) ([]RepoInstallation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, installation_id FROM installation_repos ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RepoInstallation
	for rows.Next() {
		var ri RepoInstallation
		if err := rows.Scan(&ri.Repo, &ri.InstallationID); err != nil {
			return nil, err
		}
		out = append(out, ri)
	}
	return out, rows.Err()
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
INSERT INTO jobs (id, repo, job_name, run_id, run_attempt, workflow_name, action, labels, dedupe_key, payload_json, status, conclusion, runner_id, runner_name)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(dedupe_key) DO NOTHING`
	res, err := s.db.ExecContext(ctx, q,
		j.ID, j.Repo, j.JobName, j.RunID, j.RunAttempt, j.WorkflowName, j.Action, j.Labels, j.DedupeKey, j.PayloadJSON,
		j.Status, j.Conclusion, j.RunnerID, j.RunnerName)
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
	const q = `SELECT id, repo, job_name, run_id, run_attempt, workflow_name, action, labels, dedupe_key, payload_json, status, conclusion,
		runner_id, runner_name, received_at, updated_at FROM jobs WHERE id = ?`
	var j Job
	err := s.db.QueryRowContext(ctx, q, jobID).Scan(
		&j.ID, &j.Repo, &j.JobName, &j.RunID, &j.RunAttempt, &j.WorkflowName, &j.Action, &j.Labels, &j.DedupeKey, &j.PayloadJSON, &j.Status, &j.Conclusion,
		&j.RunnerID, &j.RunnerName, &j.ReceivedAt, &j.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// dispatchedReplayAge bounds how stale a 'dispatched' job can be before
// PendingJobs replays it. The dispatch path's normal completion is the
// real `workflow_job: in_progress` webhook flipping the row to
// 'in_progress' — typically within seconds. If that never arrives
// (GitHub assigned the runner to a different job, our process crashed
// after MarkJobDispatched but before launch acked, etc.), replay rescues
// the job after this window.
const dispatchedReplayAge = 5 * time.Minute

func (s *SQLite) MarkJobDispatched(ctx context.Context, jobID int64) error {
	// Also refreshes updated_at when the row is already 'dispatched',
	// so a periodic replay that re-dispatches a stale row resets its
	// replay window — otherwise the next tick would re-fire immediately.
	const q = `UPDATE jobs SET status='dispatched',
		updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN ('pending','dispatched')`
	_, err := s.db.ExecContext(ctx, q, jobID)
	return err
}

func (s *SQLite) MarkJobInProgress(ctx context.Context, jobID int64, runnerID int64, runnerName string) (bool, error) {
	const q = `UPDATE jobs SET status='in_progress', runner_id=?, runner_name=?,
		updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN ('pending','dispatched')`
	res, err := s.db.ExecContext(ctx, q, runnerID, runnerName, jobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkJobCompleted is idempotent against an already-completed row:
// the WHERE status<>'completed' guard prevents a late writer
// (duplicate webhook delivery, or the stale-in_progress reconcile
// sweep racing the real webhook) from clobbering the conclusion that
// won the race. Returns whether a transition actually happened.
func (s *SQLite) MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) (bool, error) {
	const q = `UPDATE jobs SET status='completed', conclusion=?,
		updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status<>'completed'`
	res, err := s.db.ExecContext(ctx, q, conclusion, jobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CancelJobIfPending status-guards the cancel: only rows still in
// 'pending'/'dispatched' transition. A concurrent
// `workflow_job: completed` webhook that already wrote the real
// conclusion is not overwritten. Returns whether a row actually
// transitioned so callers can log the no-op case.
func (s *SQLite) CancelJobIfPending(ctx context.Context, jobID int64) (bool, error) {
	const q = `UPDATE jobs SET status='completed', conclusion='cancelled',
		updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status IN ('pending','dispatched')`
	res, err := s.db.ExecContext(ctx, q, jobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *SQLite) RetryJobIfCompleted(ctx context.Context, jobID int64) (bool, error) {
	const q = `UPDATE jobs SET status='pending', conclusion='', runner_id=0, runner_name='',
		action='queued', updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND status='completed'`
	res, err := s.db.ExecContext(ctx, q, jobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CancelPendingJobsForRepo transitions every still-dispatchable row
// for the repo (status in 'pending'/'dispatched') to
// completed/cancelled. The status filter is what makes this safe to
// call concurrently with a real workflow_job: completed webhook for
// any individual row — we never overwrite an already-terminal status.
func (s *SQLite) CancelPendingJobsForRepo(ctx context.Context, repoFullName string) (int64, error) {
	const q = `UPDATE jobs SET status='completed', conclusion='cancelled',
		updated_at=CURRENT_TIMESTAMP
		WHERE repo=? AND status IN ('pending','dispatched')`
	res, err := s.db.ExecContext(ctx, q, repoFullName)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PendingJobs returns rows still owed dispatch work: anything in
// 'pending', plus 'dispatched' rows whose updated_at is older than
// dispatchedReplayAge (the runner we launched never claimed a job —
// see the dispatchedReplayAge comment).
func (s *SQLite) PendingJobs(ctx context.Context) ([]*Job, error) {
	const q = `SELECT id, repo, job_name, run_id, run_attempt, workflow_name, action, labels, dedupe_key, payload_json, status, conclusion,
		runner_id, runner_name, received_at, updated_at
		FROM jobs
		WHERE status='pending'
		   OR (status='dispatched' AND updated_at < datetime('now', ?))
		ORDER BY received_at, id`
	rows, err := s.db.QueryContext(ctx, q, fmt.Sprintf("-%d seconds", int(dispatchedReplayAge.Seconds())))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Repo, &j.JobName, &j.RunID, &j.RunAttempt, &j.WorkflowName, &j.Action, &j.Labels, &j.DedupeKey, &j.PayloadJSON, &j.Status, &j.Conclusion,
			&j.RunnerID, &j.RunnerName, &j.ReceivedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &j)
	}
	return out, rows.Err()
}

func (s *SQLite) ListJobs(ctx context.Context, f JobListFilter) ([]*Job, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}

	var b strings.Builder
	b.WriteString(`SELECT id, repo, job_name, run_id, run_attempt, workflow_name, action, labels, dedupe_key, payload_json, status, conclusion,
		runner_id, runner_name, received_at, updated_at FROM jobs`)

	args := make([]any, 0, len(f.Statuses)+2)
	where := make([]string, 0, 2)
	if len(f.Statuses) > 0 {
		ph := make([]string, 0, len(f.Statuses))
		for _, st := range f.Statuses {
			if strings.TrimSpace(st) == "" {
				continue
			}
			ph = append(ph, "?")
			args = append(args, st)
		}
		if len(ph) > 0 {
			where = append(where, "status IN ("+strings.Join(ph, ",")+")")
		}
	}
	if repo := strings.TrimSpace(f.Repo); repo != "" {
		where = append(where, "repo = ?")
		args = append(args, repo)
	}
	if len(where) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(where, " AND "))
	}
	b.WriteString(" ORDER BY updated_at DESC, id DESC LIMIT ")
	b.WriteString(strconv.Itoa(limit))

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*Job, 0, limit)
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Repo, &j.JobName, &j.RunID, &j.RunAttempt, &j.WorkflowName, &j.Action, &j.Labels, &j.DedupeKey, &j.PayloadJSON, &j.Status, &j.Conclusion,
			&j.RunnerID, &j.RunnerName, &j.ReceivedAt, &j.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &j)
	}
	return out, rows.Err()
}

func (s *SQLite) Summary(ctx context.Context) (*Summary, error) {
	jobs, err := countByStatus(ctx, s.db, "jobs")
	if err != nil {
		return nil, err
	}
	runners, err := countByStatus(ctx, s.db, "runners")
	if err != nil {
		return nil, err
	}
	return &Summary{
		JobsByStatus:    jobs,
		RunnersByStatus: runners,
	}, nil
}

func countByStatus(ctx context.Context, db *sql.DB, table string) (map[string]int64, error) {
	var q string
	switch table {
	case "jobs":
		q = `SELECT status, count(*) FROM jobs GROUP BY status`
	case "runners":
		q = `SELECT status, count(*) FROM runners GROUP BY status`
	default:
		return nil, fmt.Errorf("countByStatus: unknown table %q", table)
	}
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
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
