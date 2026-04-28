package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muhac/actions-runner-pool/internal/config"
	"github.com/muhac/actions-runner-pool/internal/runner"
	"github.com/muhac/actions-runner-pool/internal/store"
)

// --- helpers -----------------------------------------------------------------

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestScheduler(t *testing.T, cfg *config.Config, st store.Store, gh GitHubClient, ln Launcher) *Scheduler {
	t.Helper()
	s := New(cfg, st, gh, ln, discardLogger())
	s.capBackoff = 1 * time.Millisecond
	s.nameFn = func(jobID int64) (string, string) {
		name := "test-runner"
		return name, name
	}
	return s
}

func newCfg(maxConcurrent int) *config.Config {
	return &config.Config{
		MaxConcurrentRunners: maxConcurrent,
		RunnerImage:          "test-image:latest",
	}
}

// --- spy GitHub client -------------------------------------------------------

type spyGH struct {
	jwtCalls    atomic.Int64
	instCalls   atomic.Int64
	regCalls    atomic.Int64
	regErr      error
	callOrder   []string
}

func (g *spyGH) AppJWT(pem []byte, appID int64) (string, error) {
	g.jwtCalls.Add(1)
	g.callOrder = append(g.callOrder, "AppJWT")
	return "jwt", nil
}

func (g *spyGH) InstallationToken(ctx context.Context, jwt string, installationID int64) (string, error) {
	g.instCalls.Add(1)
	g.callOrder = append(g.callOrder, "InstallationToken")
	return "inst-token", nil
}

func (g *spyGH) RegistrationToken(ctx context.Context, instTok, repo string) (string, error) {
	g.regCalls.Add(1)
	g.callOrder = append(g.callOrder, "RegistrationToken")
	if g.regErr != nil {
		return "", g.regErr
	}
	return "reg-token", nil
}

// --- spy launcher ------------------------------------------------------------

type spyLauncher struct {
	calls   atomic.Int64
	lastSpec runner.Spec
	err     error
}

func (l *spyLauncher) Launch(ctx context.Context, spec runner.Spec) error {
	l.calls.Add(1)
	l.lastSpec = spec
	return l.err
}

// --- in-memory-ish store wrappers via SQLite ---------------------------------
//
// We reuse the real SQLite store (in-memory file via tempdir) so test setup
// matches production wiring. Tests focus on dispatch behavior, not store
// behavior.

func newStoreT(t *testing.T) *store.SQLite {
	t.Helper()
	s, err := store.OpenSQLite("file:" + t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedAppConfig(t *testing.T, st store.Store) {
	t.Helper()
	if err := st.SaveAppConfig(context.Background(), &store.AppConfig{
		AppID: 1, Slug: "gharp", WebhookSecret: "x", PEM: []byte("pem"),
		ClientID: "Iv1.x", BaseURL: "https://example.test",
	}); err != nil {
		t.Fatal(err)
	}
}

func seedInstallation(t *testing.T, st store.Store, instID int64, repo string) {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertInstallation(ctx, &store.Installation{
		ID: instID, AccountID: 100, AccountLogin: "owner", AccountType: "User",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRepoInstallation(ctx, repo, instID); err != nil {
		t.Fatal(err)
	}
}

func seedPendingJob(t *testing.T, st store.Store, jobID int64, repo string) {
	t.Helper()
	if _, err := st.InsertJobIfNew(context.Background(), &store.Job{
		ID:        jobID,
		Repo:      repo,
		Action:    "queued",
		Labels:    "self-hosted",
		DedupeKey: repo + "/" + itoa(jobID),
		Status:    "pending",
	}); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// --- replay tests ------------------------------------------------------------

func TestReplay_EnqueuesAllPending(t *testing.T) {
	st := newStoreT(t)
	for _, id := range []int64{10, 20, 30} {
		seedPendingJob(t, st, id, "owner/repo")
	}

	s := newTestScheduler(t, newCfg(8), st, &spyGH{}, &spyLauncher{})
	s.replay(context.Background())

	got := drainChan(s.jobCh)
	if len(got) != 3 {
		t.Fatalf("enqueued %d jobs, want 3 (got=%v)", len(got), got)
	}
}

func TestReplay_OverflowLeavesPending(t *testing.T) {
	st := newStoreT(t)
	for i := int64(1); i <= 300; i++ {
		seedPendingJob(t, st, i, "owner/repo")
	}

	s := newTestScheduler(t, newCfg(8), st, &spyGH{}, &spyLauncher{})
	s.replay(context.Background())

	if got := len(s.jobCh); got != 256 {
		t.Fatalf("channel depth %d, want 256 (full)", got)
	}
	// The 44 over-capacity jobs are left for the next startup; nothing
	// should have panicked. Drain to confirm we can read what we enqueued.
	got := drainChan(s.jobCh)
	if len(got) != 256 {
		t.Fatalf("drained %d, want 256", len(got))
	}
}

// --- dispatch tests ----------------------------------------------------------

func TestDispatch_HappyPath_InsertsStartingRunnerAndLaunches(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")

	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(4), st, gh, ln)

	s.dispatch(context.Background(), 1)

	if ln.calls.Load() != 1 {
		t.Fatalf("Launch calls = %d, want 1", ln.calls.Load())
	}
	if ln.lastSpec.ContainerName != "test-runner" || ln.lastSpec.RunnerName != "test-runner" {
		t.Fatalf("spec names = %+v, want both 'test-runner'", ln.lastSpec)
	}
	if ln.lastSpec.RegistrationToken != "reg-token" {
		t.Fatalf("spec.RegistrationToken = %q, want reg-token", ln.lastSpec.RegistrationToken)
	}
	if ln.lastSpec.RepoURL != "https://github.com/owner/repo" {
		t.Fatalf("spec.RepoURL = %q", ln.lastSpec.RepoURL)
	}

	runners, err := st.ListActiveRunners(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 || runners[0].Status != "starting" {
		t.Fatalf("runners=%+v, want one starting row", runners)
	}
	if runners[0].ContainerName != "test-runner" || runners[0].RunnerName != "test-runner" {
		t.Fatalf("runner row missing names: %+v", runners[0])
	}
}

func TestDispatch_ConcurrencyCap_RequeuesWithoutMintingTokens(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")

	// Cap = 1, and there's already 1 active runner.
	if err := st.InsertRunner(context.Background(), &store.Runner{
		ContainerName: "preexisting",
		Repo:          "owner/repo",
		RunnerName:    "preexisting",
		Labels:        "self-hosted",
		Status:        "starting",
	}); err != nil {
		t.Fatal(err)
	}

	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(1), st, gh, ln)

	s.dispatch(context.Background(), 1)

	if gh.regCalls.Load() != 0 || gh.instCalls.Load() != 0 || gh.jwtCalls.Load() != 0 {
		t.Fatalf("token mints called under cap: jwt=%d inst=%d reg=%d",
			gh.jwtCalls.Load(), gh.instCalls.Load(), gh.regCalls.Load())
	}
	if ln.calls.Load() != 0 {
		t.Fatalf("Launch called under cap: %d", ln.calls.Load())
	}
	// The job must be re-enqueued for a later attempt.
	if got := <-s.jobCh; got != 1 {
		t.Fatalf("expected job 1 re-enqueued, got %d", got)
	}
}

func TestDispatch_NoInstallation_StaysPending(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedPendingJob(t, st, 1, "owner/repo") // no installation row

	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(4), st, gh, ln)

	s.dispatch(context.Background(), 1)

	if gh.jwtCalls.Load() != 0 {
		t.Fatalf("AppJWT called without an installation: %d", gh.jwtCalls.Load())
	}
	if ln.calls.Load() != 0 {
		t.Fatalf("Launch called without an installation: %d", ln.calls.Load())
	}
	job, _ := st.GetJob(context.Background(), 1)
	if job == nil || job.Status != "pending" {
		t.Fatalf("job status=%v, want pending", job)
	}
}

func TestDispatch_LaunchError_MarksFinished(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")

	gh := &spyGH{}
	ln := &spyLauncher{err: errors.New("docker boom")}
	s := newTestScheduler(t, newCfg(4), st, gh, ln)

	s.dispatch(context.Background(), 1)

	// ListActiveRunners filters to starting/idle/busy — finished should not appear.
	active, _ := st.ListActiveRunners(context.Background())
	if len(active) != 0 {
		t.Fatalf("active runners after launch error = %d, want 0", len(active))
	}
}

func TestDispatch_TokenOrder_CapBeforeJWT(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")

	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(4), st, gh, ln)

	s.dispatch(context.Background(), 1)

	// JWT (and downstream) must have run AFTER the cap check; we can't
	// observe the cap call directly without a store spy, but we can assert
	// the dispatch reached the mint stage at all (cap was below 4 here).
	if gh.jwtCalls.Load() != 1 || gh.instCalls.Load() != 1 || gh.regCalls.Load() != 1 {
		t.Fatalf("expected one of each mint, got jwt=%d inst=%d reg=%d",
			gh.jwtCalls.Load(), gh.instCalls.Load(), gh.regCalls.Load())
	}
	// Order within github calls: AppJWT before InstallationToken before RegistrationToken.
	want := []string{"AppJWT", "InstallationToken", "RegistrationToken"}
	if len(gh.callOrder) != 3 {
		t.Fatalf("callOrder=%v, want 3 entries", gh.callOrder)
	}
	for i := range want {
		if gh.callOrder[i] != want[i] {
			t.Fatalf("callOrder[%d]=%q, want %q (full=%v)", i, gh.callOrder[i], want[i], gh.callOrder)
		}
	}
}

func TestDispatch_NonPendingJob_Skips(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")
	if err := st.MarkJobInProgress(context.Background(), 1, 7, "real-runner"); err != nil {
		t.Fatal(err)
	}

	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(4), st, gh, ln)

	s.dispatch(context.Background(), 1)

	if ln.calls.Load() != 0 {
		t.Fatalf("Launch called for non-pending job: %d", ln.calls.Load())
	}
	if gh.jwtCalls.Load() != 0 {
		t.Fatalf("token minted for non-pending job: %d", gh.jwtCalls.Load())
	}
}

// --- utility ----------------------------------------------------------------

func drainChan(ch chan int64) []int64 {
	var out []int64
	for {
		select {
		case v := <-ch:
			out = append(out, v)
		default:
			return out
		}
	}
}
