//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muhac/actions-runner-pool/internal/store"
)

const (
	itWebhookSecret = "integration-secret"
)

// startBinary builds + boots the binary with the given env, returning the URL
// it listens on. The DSN is also returned so the test can read it back later.
func startBinary(t *testing.T) (baseURL, dbPath string) {
	t.Helper()
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "gharp")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(tmp, "it.db")

	// Pre-seed app_config so the webhook handler accepts signed requests.
	seedAppConfig(t, dbPath)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = append(os.Environ(),
		"PORT="+port,
		"BASE_URL=http://127.0.0.1:"+port,
		"STORE_DSN=file:"+dbPath,
		"LOG_LEVEL=warn",
		// Isolate the reconciler's orphan sweep to a per-test
		// namespace. Without this, the binary's reconciler would
		// scan the host docker daemon for "gharp-" containers and
		// happily docker rm -f ANY container on the host with that
		// prefix — including the self-hosted runner the test itself
		// is running in. Using a unique prefix keeps the sweep
		// confined to containers this test would have created.
		"RUNNER_NAME_PREFIX=gharp-it-"+t.Name()+"-",
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

	baseURL = "http://127.0.0.1:" + port
	if err := waitForHTTPClient(baseURL+"/healthz", 5*time.Second); err != nil {
		t.Fatalf("healthz never up: %v", err)
	}
	return baseURL, dbPath
}

func seedAppConfig(t *testing.T, dbPath string) {
	t.Helper()
	st, err := store.OpenSQLite("file:" + dbPath)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.SaveAppConfig(context.Background(), &store.AppConfig{
		AppID: 1, Slug: "gharp-it", WebhookSecret: itWebhookSecret,
		PEM: []byte("test"), ClientID: "Iv1.test", BaseURL: "http://127.0.0.1:0",
	}); err != nil {
		t.Fatalf("seed app_config: %v", err)
	}
}

func waitForHTTPClient(url string, timeout time.Duration) error {
	c := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := c.Get(url)
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

func signIT(body []byte) string {
	mac := hmac.New(sha256.New, []byte(itWebhookSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postEvent(t *testing.T, baseURL, event string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/github/webhook", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signIT(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestIntegration_InstallationEvent_PopulatesTables(t *testing.T) {
	baseURL, dbPath := startBinary(t)

	body, _ := json.Marshal(map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id":      99,
			"account": map[string]any{"id": 7, "login": "alice", "type": "User"},
		},
		"repositories": []map[string]any{
			{"full_name": "alice/repo1"},
			{"full_name": "alice/repo2"},
		},
	})
	resp := postEvent(t, baseURL, "installation", body)
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, body = %s", resp.StatusCode, buf)
	}
	_ = resp.Body.Close()

	// Open the live DB directly to inspect side effects.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var instID int64
	if err := db.QueryRow(`SELECT id FROM installations WHERE id=99`).Scan(&instID); err != nil {
		t.Fatalf("installations row missing: %v", err)
	}
	for _, repo := range []string{"alice/repo1", "alice/repo2"} {
		var got int64
		if err := db.QueryRow(`SELECT installation_id FROM installation_repos WHERE repo=?`, repo).Scan(&got); err != nil {
			t.Errorf("repo %q missing: %v", repo, err)
		} else if got != 99 {
			t.Errorf("repo %q -> installation_id %d, want 99", repo, got)
		}
	}
}

func TestIntegration_WorkflowJobQueued_InsertsJob(t *testing.T) {
	baseURL, dbPath := startBinary(t)

	// First seed the installation->repo mapping so InstallationForRepo works
	// in any future flows; webhook also lazy-writes it but doing it here
	// makes the test order-independent.
	instBody, _ := json.Marshal(map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id":      99,
			"account": map[string]any{"id": 7, "login": "alice", "type": "User"},
		},
		"repositories": []map[string]any{{"full_name": "alice/repo"}},
	})
	resp := postEvent(t, baseURL, "installation", instBody)
	_ = resp.Body.Close()

	jobBody, _ := json.Marshal(map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id":     1234567,
			"labels": []string{"self-hosted"},
		},
		"repository":   map[string]any{"full_name": "alice/repo", "private": true},
		"installation": map[string]any{"id": 99},
	})
	resp = postEvent(t, baseURL, "workflow_job", jobBody)
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, body = %s", resp.StatusCode, buf)
	}
	_ = resp.Body.Close()

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var status, repo string
	if err := db.QueryRow(`SELECT status, repo FROM jobs WHERE id=1234567`).Scan(&status, &repo); err != nil {
		t.Fatalf("jobs row missing: %v", err)
	}
	if status != "pending" || repo != "alice/repo" {
		t.Errorf("got status=%s repo=%s", status, repo)
	}

	// Re-deliver same event — dedup at store should keep status pending and
	// not insert a second row (jobs.id is the PK).
	resp = postEvent(t, baseURL, "workflow_job", jobBody)
	_ = resp.Body.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM jobs WHERE id=1234567`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("dedup failed: %d rows", count)
	}
}

func TestIntegration_BadSignature_401(t *testing.T) {
	baseURL, _ := startBinary(t)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/github/webhook", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// freePort is also defined in main_smoke_test.go; we redeclare locally
// because the two test files have disjoint build tags (smoke vs
// integration) and are never compiled together.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p, nil
}
