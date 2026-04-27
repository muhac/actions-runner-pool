//go:build smoke

package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestSmoke_BinaryStartsAndOpensDB builds the binary, starts it against a
// temp DSN, hits /healthz, and asserts that all 5 expected tables were
// created in the sqlite file. Opt-in via `go test -tags smoke ./cmd/gharp`.
func TestSmoke_BinaryStartsAndOpensDB(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "gharp")

	build := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(tmp, "smoke.db")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = append(cmd.Env,
		"PORT="+port,
		"BASE_URL=http://127.0.0.1:"+port,
		"STORE_DSN=file:"+dbPath,
		"LOG_LEVEL=warn",
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})

	if err := waitForHTTP("http://127.0.0.1:"+port+"/healthz", 5*time.Second); err != nil {
		t.Fatalf("healthz never up: %v", err)
	}

	got := tablesIn(t, dbPath)
	want := []string{"app_config", "installation_repos", "installations", "jobs", "runners"}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("table %q missing; got %v", w, got)
		}
	}
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p, nil
}

func waitForHTTP(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("timeout")
}

func tablesIn(t *testing.T, dbPath string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
