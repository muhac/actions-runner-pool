package store

import (
	"context"
	"testing"
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
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	_ = rows.Close()
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

	s2, err := OpenSQLite(dsn)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
}
