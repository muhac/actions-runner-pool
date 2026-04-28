//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muhac/actions-runner-pool/internal/store"
)

// TestIntegration_QueuedJob_DispatchesRunner exercises the Phase 4 hot path
// end-to-end: signed `workflow_job: queued` webhook → store → scheduler
// replay/dispatch → fake GitHub API mints tokens → launcher exec is invoked.
//
// `docker` is replaced by `/usr/bin/true` (or the platform equivalent) via
// the RUNNER_COMMAND env override, so this test does not require a docker
// daemon. The fake GitHub API serves installation + registration tokens.
func TestIntegration_QueuedJob_DispatchesRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("RUNNER_COMMAND substitution uses a unix path")
	}

	// 1. RSA key for the App JWT signature path.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	// 2. Fake GitHub API. Counts hits per endpoint so the test can assert
	//    the dispatch sequence ran against it.
	var instTokenHits, regTokenHits atomic.Int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/99/access_tokens":
			instTokenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "inst-token",
				"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
			})
		case r.URL.Path == "/repos/alice/repo/actions/runners/registration-token":
			regTokenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "reg-token",
				"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
			})
		default:
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer gh.Close()

	// 3. Boot binary with RUNNER_COMMAND replaced by /usr/bin/true so
	//    "launching a container" is a no-op syscall — but the call still
	//    has to happen for InsertRunner -> Launch to complete.
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "gharp")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(tmp, "it.db")

	// Pre-seed app_config with the real PEM and a known webhook secret.
	st, err := store.OpenSQLite("file:" + dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAppConfig(context.Background(), &store.AppConfig{
		AppID: 1, Slug: "gharp-it", WebhookSecret: itWebhookSecret,
		PEM: pemBytes, ClientID: "Iv1.test", BaseURL: "http://127.0.0.1:" + port,
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	// RUNNER_COMMAND must contain every required placeholder per
	// config.requiredPlaceholders, even if /usr/bin/true ignores them.
	runnerCmd, _ := json.Marshal([]string{
		"/usr/bin/true",
		"{{.ContainerName}}",
		"{{.RegistrationToken}}",
		"{{.RunnerName}}",
		"{{.RepoURL}}",
		"{{.Labels}}",
		"{{.Image}}",
	})

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binPath)
	cmd.Env = append(os.Environ(),
		"PORT="+port,
		"BASE_URL=http://127.0.0.1:"+port,
		"STORE_DSN=file:"+dbPath,
		"GITHUB_API_BASE="+gh.URL,
		"RUNNER_COMMAND="+string(runnerCmd),
		"MAX_CONCURRENT_RUNNERS=4",
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

	baseURL := "http://127.0.0.1:" + port
	if err := waitForHTTPClient(baseURL+"/healthz", 5*time.Second); err != nil {
		t.Fatalf("healthz never up: %v", err)
	}

	// 4. Send installation webhook so installation_repos is populated.
	instBody, _ := json.Marshal(map[string]any{
		"action": "created",
		"installation": map[string]any{
			"id":      99,
			"account": map[string]any{"id": 7, "login": "alice", "type": "User"},
		},
		"repositories": []map[string]any{{"full_name": "alice/repo"}},
	})
	resp := postEvent(t, baseURL, "installation", instBody)
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("installation webhook: status=%d body=%s", resp.StatusCode, buf)
	}
	_ = resp.Body.Close()

	// 5. Fire workflow_job: queued.
	jobID := int64(424242)
	jobBody, _ := json.Marshal(map[string]any{
		"action": "queued",
		"workflow_job": map[string]any{
			"id":     jobID,
			"labels": []string{"self-hosted"},
		},
		"repository":   map[string]any{"full_name": "alice/repo"},
		"installation": map[string]any{"id": 99},
	})
	resp = postEvent(t, baseURL, "workflow_job", jobBody)
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("workflow_job webhook: status=%d body=%s", resp.StatusCode, buf)
	}
	_ = resp.Body.Close()

	// 6. Poll the DB for a runners row. Dispatch is async so we wait.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	var (
		containerName string
		runnerName    string
		runnerStatus  string
	)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := db.QueryRow(
			`SELECT container_name, runner_name, status FROM runners WHERE repo='alice/repo' LIMIT 1`,
		).Scan(&containerName, &runnerName, &runnerStatus)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if containerName == "" {
		t.Fatalf("no runners row appeared after dispatch (instTokenHits=%d regTokenHits=%d)",
			instTokenHits.Load(), regTokenHits.Load())
	}
	if runnerStatus != "starting" {
		t.Errorf("runners.status=%q, want starting", runnerStatus)
	}
	if containerName != runnerName {
		t.Errorf("container_name=%q runner_name=%q (defaultNameFn returns same)", containerName, runnerName)
	}
	if instTokenHits.Load() != 1 {
		t.Errorf("installation token mints=%d, want 1", instTokenHits.Load())
	}
	if regTokenHits.Load() != 1 {
		t.Errorf("registration token mints=%d, want 1", regTokenHits.Load())
	}
}

// TestIntegration_StartupReplay_RecoversPendingJob seeds a `pending` job in
// the DB BEFORE the binary starts, then asserts the scheduler's startup
// replay picks it up and reaches the launch path.
func TestIntegration_StartupReplay_RecoversPendingJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("RUNNER_COMMAND substitution uses a unix path")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	var regHits atomic.Int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/77/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "inst-token",
				"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
			})
		case "/repos/bob/repo/actions/runners/registration-token":
			regHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "reg-token",
				"expires_at": time.Now().Add(1 * time.Hour).Format(time.RFC3339),
			})
		default:
			http.Error(w, "unexpected: "+r.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer gh.Close()

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "it.db")

	// Seed app_config + installation + a PENDING job before the binary boots.
	st, err := store.OpenSQLite("file:" + dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SaveAppConfig(ctx, &store.AppConfig{
		AppID: 1, Slug: "gharp-it", WebhookSecret: itWebhookSecret,
		PEM: pemBytes, ClientID: "Iv1.test", BaseURL: "http://127.0.0.1:0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertInstallation(ctx, &store.Installation{
		ID: 77, AccountID: 8, AccountLogin: "bob", AccountType: "User",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRepoInstallation(ctx, "bob/repo", 77); err != nil {
		t.Fatal(err)
	}
	jobID := int64(555555)
	if _, err := st.InsertJobIfNew(ctx, &store.Job{
		ID: jobID, Repo: "bob/repo", Action: "queued",
		Labels: "self-hosted", DedupeKey: "bob/repo/" + strconv.FormatInt(jobID, 10),
		Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	binPath := filepath.Join(tmp, "gharp")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	runnerCmd, _ := json.Marshal([]string{
		"/usr/bin/true",
		"{{.ContainerName}}", "{{.RegistrationToken}}", "{{.RunnerName}}",
		"{{.RepoURL}}", "{{.Labels}}", "{{.Image}}",
	})

	runCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(runCtx, binPath)
	cmd.Env = append(os.Environ(),
		"PORT="+port,
		"BASE_URL=http://127.0.0.1:"+port,
		"STORE_DSN=file:"+dbPath,
		"GITHUB_API_BASE="+gh.URL,
		"RUNNER_COMMAND="+string(runnerCmd),
		"MAX_CONCURRENT_RUNNERS=4",
		"LOG_LEVEL=warn",
	)
	stderr, _ := cmd.StderrPipe()
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	if err := waitForHTTPClient("http://127.0.0.1:"+port+"/healthz", 5*time.Second); err != nil {
		t.Fatalf("healthz: %v", err)
	}

	// Replay should have fired by now; poll for the runners row.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var status string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := db.QueryRow(`SELECT status FROM runners WHERE repo='bob/repo' LIMIT 1`).Scan(&status)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "starting" {
		t.Fatalf("runners row not created by replay (status=%q, regHits=%d)", status, regHits.Load())
	}
	if regHits.Load() != 1 {
		t.Errorf("registration token mints=%d, want 1", regHits.Load())
	}
}
