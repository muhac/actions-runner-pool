package store

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSchema_AppliesAndIdempotent(t *testing.T) {
	dsn := "file:" + t.TempDir() + "/test.db"
	s, err := OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	rows, err := s.db.QueryContext(context.Background(),
		"SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatalf("query tables: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration: %v", err)
	}
	want := []string{"app_config", "installation_repos", "installations", "jobs", "runners"}
	if len(got) != len(want) {
		t.Fatalf("tables = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("tables[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s3, err := OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("re-open for index check: %v", err)
	}
	idxRows, err := s3.db.QueryContext(context.Background(),
		"SELECT name FROM sqlite_master WHERE type='index' AND name LIKE 'idx_%' ORDER BY name")
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer func() { _ = idxRows.Close() }()
	var idxGot []string
	for idxRows.Next() {
		var n string
		if err := idxRows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		idxGot = append(idxGot, n)
	}
	if err := idxRows.Err(); err != nil {
		t.Fatalf("index rows iteration: %v", err)
	}
	idxWant := []string{
		"idx_installation_repos_inst",
		"idx_jobs_repo_updated",
		"idx_jobs_run_id",
		"idx_jobs_status",
		"idx_runners_runner_name",
		"idx_runners_status",
	}
	if len(idxGot) != len(idxWant) {
		t.Fatalf("indexes = %v, want %v", idxGot, idxWant)
	}
	for i, w := range idxWant {
		if idxGot[i] != w {
			t.Fatalf("indexes[%d] = %q, want %q (full: %v)", i, idxGot[i], w, idxGot)
		}
	}
	if err := s3.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSchema_MigratesOldJobsTableBeforeNewIndexes(t *testing.T) {
	dsn := "file:" + t.TempDir() + "/old.db"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `
CREATE TABLE jobs (
  id           INTEGER PRIMARY KEY,
  repo         TEXT    NOT NULL,
  action       TEXT    NOT NULL,
  labels       TEXT    NOT NULL,
  dedupe_key   TEXT    NOT NULL UNIQUE,
  status       TEXT    NOT NULL,
  conclusion   TEXT    NOT NULL DEFAULT '',
  runner_id    INTEGER NOT NULL DEFAULT 0,
  runner_name  TEXT    NOT NULL DEFAULT '',
  received_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`)
	if err != nil {
		t.Fatalf("create old jobs table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("OpenSQLite should migrate old jobs table: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO jobs (id, repo, action, labels, dedupe_key, status) VALUES (1, 'a/b', 'queued', 'x', '1', 'pending')`); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
	for _, idx := range []string{"idx_jobs_repo_updated", "idx_jobs_run_id"} {
		var name string
		err := s.db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&name)
		if err != nil {
			t.Fatalf("index %s missing after migration: %v", idx, err)
		}
	}
}
