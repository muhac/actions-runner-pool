package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
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
	jwtErr      error
	instErr     error
	regErr      error
	mu          sync.Mutex
	callOrder   []string
}

func (g *spyGH) recordCall(name string) {
	g.mu.Lock()
	g.callOrder = append(g.callOrder, name)
	g.mu.Unlock()
}

func (g *spyGH) order() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.callOrder))
	copy(out, g.callOrder)
	return out
}

func (g *spyGH) AppJWT(pem []byte, appID int64) (string, error) {
	g.jwtCalls.Add(1)
	g.recordCall("AppJWT")
	if g.jwtErr != nil {
		return "", g.jwtErr
	}
	return "jwt", nil
}

func (g *spyGH) InstallationToken(ctx context.Context, jwt string, installationID int64) (string, error) {
	g.instCalls.Add(1)
	g.recordCall("InstallationToken")
	if g.instErr != nil {
		return "", g.instErr
	}
	return "inst-token", nil
}

func (g *spyGH) RegistrationToken(ctx context.Context, instTok, repo string) (string, error) {
	g.regCalls.Add(1)
	g.recordCall("RegistrationToken")
	if g.regErr != nil {
		return "", g.regErr
	}
	return "reg-token", nil
}

// --- spy launcher ------------------------------------------------------------

type spyLauncher struct {
	calls    atomic.Int64
	mu       sync.Mutex
	lastSpec runner.Spec
	err      error
}

func (l *spyLauncher) Launch(ctx context.Context, spec runner.Spec) error {
	l.calls.Add(1)
	l.mu.Lock()
	l.lastSpec = spec
	l.mu.Unlock()
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
	if runners[0].StartedAt.IsZero() {
		t.Fatalf("runner row started_at is zero (year 0001) — must be set on insert")
	}

	// Job must be advanced past 'pending' so a restart's replay won't
	// re-dispatch it. Binding stays 0/"" because the webhook hasn't
	// landed yet — those columns are written later by MarkJobInProgress.
	job, _ := st.GetJob(context.Background(), 1)
	if job == nil || job.Status != "in_progress" {
		t.Fatalf("job status=%v, want in_progress after launch", job)
	}
	if job.RunnerID != 0 || job.RunnerName != "" {
		t.Fatalf("job binding=%d/%q, want unset 0/\"\" before webhook", job.RunnerID, job.RunnerName)
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
	// Re-enqueue is async (time.AfterFunc) — capBackoff is 1ms in tests
	// so the read below should arrive promptly.
	select {
	case got := <-s.jobCh:
		if got != 1 {
			t.Fatalf("expected job 1 re-enqueued, got %d", got)
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("expected job 1 re-enqueued within 1s")
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
	order := gh.order()
	if len(order) != 3 {
		t.Fatalf("callOrder=%v, want 3 entries", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("callOrder[%d]=%q, want %q (full=%v)", i, order[i], want[i], order)
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

// --- additional coverage -----------------------------------------------------

// (1) Run loop: replays then drains channel; returns context.Canceled on stop.
func TestRun_DispatchesEnqueuedAfterReplay(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo") // replayed
	seedPendingJob(t, st, 2, "owner/repo") // also replayed

	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(8), st, gh, ln)
	// Make container/runner names unique per call so InsertRunner doesn't
	// PK-conflict between the two dispatches.
	var nameCounter atomic.Int64
	s.nameFn = func(jobID int64) (string, string) {
		n := nameCounter.Add(1)
		name := "test-runner-" + itoa(n)
		return name, name
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait until both replayed jobs have been dispatched. We poll on the
	// launcher counter so the test is deterministic without sleeping.
	deadline := time.Now().Add(2 * time.Second)
	for ln.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if ln.calls.Load() != 2 {
		t.Fatalf("Launch calls after replay = %d, want 2", ln.calls.Load())
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

// (2) cap-backoff vs ctx cancel: ctx cancelled while sleeping → no re-enqueue.
func TestDispatch_CapBackoffCancelled_NoRequeue(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")
	if err := st.InsertRunner(context.Background(), &store.Runner{
		ContainerName: "preexisting",
		Repo:          "owner/repo",
		RunnerName:    "preexisting",
		Labels:        "self-hosted",
		Status:        "starting",
	}); err != nil {
		t.Fatal(err)
	}

	s := newTestScheduler(t, newCfg(1), st, &spyGH{}, &spyLauncher{})
	s.capBackoff = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	s.dispatch(ctx, 1)
	// dispatch schedules an AfterFunc and returns immediately. Cancel
	// before AfterFunc fires; assert it respects the cancellation.
	cancel()
	time.Sleep(150 * time.Millisecond) // > capBackoff so AfterFunc has fired

	if got := len(s.jobCh); got != 0 {
		t.Fatalf("channel depth=%d, want 0 (ctx cancel must skip re-enqueue)", got)
	}
}

// (3) GitHub three-stage failures: each layer's error must short-circuit.
func TestDispatch_GitHubErrors_ShortCircuit(t *testing.T) {
	cases := []struct {
		name           string
		mutate         func(g *spyGH)
		wantInstCalled bool
		wantRegCalled  bool
	}{
		{"AppJWT_fails", func(g *spyGH) { g.jwtErr = errors.New("jwt boom") }, false, false},
		{"InstallationToken_fails", func(g *spyGH) { g.instErr = errors.New("inst boom") }, true, false},
		{"RegistrationToken_fails", func(g *spyGH) { g.regErr = errors.New("reg boom") }, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStoreT(t)
			seedAppConfig(t, st)
			seedInstallation(t, st, 999, "owner/repo")
			seedPendingJob(t, st, 1, "owner/repo")

			gh := &spyGH{}
			tc.mutate(gh)
			ln := &spyLauncher{}
			s := newTestScheduler(t, newCfg(4), st, gh, ln)

			s.dispatch(context.Background(), 1)

			if (gh.instCalls.Load() > 0) != tc.wantInstCalled {
				t.Fatalf("InstallationToken called=%v, want=%v", gh.instCalls.Load() > 0, tc.wantInstCalled)
			}
			if (gh.regCalls.Load() > 0) != tc.wantRegCalled {
				t.Fatalf("RegistrationToken called=%v, want=%v", gh.regCalls.Load() > 0, tc.wantRegCalled)
			}
			if ln.calls.Load() != 0 {
				t.Fatalf("Launch called despite github error: %d", ln.calls.Load())
			}
			active, _ := st.ListActiveRunners(context.Background())
			if len(active) != 0 {
				t.Fatalf("active runners after github error = %d, want 0", len(active))
			}
		})
	}
}

// (4) GetAppConfig returns nil → bail without minting.
func TestDispatch_NoAppConfig_StaysPending(t *testing.T) {
	st := newStoreT(t)
	// Do NOT seed app config.
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")

	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(4), st, gh, ln)

	s.dispatch(context.Background(), 1)

	if gh.jwtCalls.Load() != 0 {
		t.Fatalf("AppJWT called without app_config: %d", gh.jwtCalls.Load())
	}
	if ln.calls.Load() != 0 {
		t.Fatalf("Launch called without app_config: %d", ln.calls.Load())
	}
}

// (5) InsertRunner conflict (PK collision) → no Launch.
func TestDispatch_InsertRunnerConflict_NoLaunch(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")
	// Pre-occupy the container_name primary key the test nameFn will produce.
	if err := st.InsertRunner(context.Background(), &store.Runner{
		ContainerName: "test-runner",
		Repo:          "owner/repo",
		RunnerName:    "test-runner",
		Labels:        "self-hosted",
		Status:        "finished", // not active, so cap check still passes
	}); err != nil {
		t.Fatal(err)
	}

	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(4), st, gh, ln)

	s.dispatch(context.Background(), 1)

	if ln.calls.Load() != 0 {
		t.Fatalf("Launch called despite InsertRunner conflict: %d", ln.calls.Load())
	}
}

// (6) Channel-full re-enqueue logs warn but does not panic; job still in
// sqlite as pending.
func TestDispatch_CapRequeueWhenChannelFull(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")
	// One active runner = at cap (cfg=1).
	if err := st.InsertRunner(context.Background(), &store.Runner{
		ContainerName: "preexisting", Repo: "owner/repo",
		RunnerName: "preexisting", Labels: "x", Status: "starting",
	}); err != nil {
		t.Fatal(err)
	}

	s := newTestScheduler(t, newCfg(1), st, &spyGH{}, &spyLauncher{})
	// Saturate the channel so the cap-path Enqueue takes the default branch.
	for i := 0; i < cap(s.jobCh); i++ {
		s.jobCh <- int64(1000 + i)
	}

	s.dispatch(context.Background(), 1)

	// Job 1 stays pending in sqlite; the dropped Enqueue is recoverable on
	// next replay.
	job, _ := st.GetJob(context.Background(), 1)
	if job == nil || job.Status != "pending" {
		t.Fatalf("job status=%v, want pending", job)
	}
}

// (7) PendingJobs error → replay logs and returns; no panic.
func TestReplay_StoreErrorDoesNotPanic(t *testing.T) {
	s := newTestScheduler(t, newCfg(4), &errStore{pendingErr: errors.New("db down")}, &spyGH{}, &spyLauncher{})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("replay panicked: %v", r)
		}
	}()
	s.replay(context.Background())
	if got := len(s.jobCh); got != 0 {
		t.Fatalf("channel depth=%d, want 0 on PendingJobs error", got)
	}
}

// (8) Labels: runner registers with job.Labels, not cfg.RunnerLabels.
// cfg.RunnerLabels is admission filter only (applied at the webhook), so
// dispatch must register the runner with the labels GitHub will look for
// when matching the queued job.
func TestDispatch_RunnerLabelsCameFromJob(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	// seedPendingJob writes job.Labels = "self-hosted"; override below.
	seedPendingJob(t, st, 1, "owner/repo")
	if _, err := st.InsertJobIfNew(context.Background(), &store.Job{
		ID: 2, Repo: "owner/repo", Action: "queued",
		Labels: "self-hosted,linux,x64", DedupeKey: "owner/repo/2", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	cfg := newCfg(4)
	// cfg.RunnerLabels is non-empty AND different from job.Labels — the
	// fix is: ignore cfg here, use job's full label set.
	cfg.RunnerLabels = []string{"self-hosted"}
	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, cfg, st, gh, ln)

	s.dispatch(context.Background(), 2)

	want := "self-hosted,linux,x64"
	if ln.lastSpec.Labels != want {
		t.Fatalf("Launch spec.Labels=%q, want %q (must come from job, not cfg)", ln.lastSpec.Labels, want)
	}
	active, _ := st.ListActiveRunners(context.Background())
	if len(active) != 1 || active[0].Labels != want {
		t.Fatalf("runners.labels=%q, want %q", active[0].Labels, want)
	}
}

// (9) Concurrent dispatch of same jobID: only one Launch wins (PK guard on
// runners.container_name keeps the second from launching).
func TestDispatch_ConcurrentSameJobID_AtMostOneLaunch(t *testing.T) {
	st := newStoreT(t)
	seedAppConfig(t, st)
	seedInstallation(t, st, 999, "owner/repo")
	seedPendingJob(t, st, 1, "owner/repo")

	// Both goroutines must produce the SAME container_name so the PK
	// collision exposes any missing serialization.
	gh := &spyGH{}
	ln := &spyLauncher{}
	s := newTestScheduler(t, newCfg(8), st, gh, ln)
	s.nameFn = func(jobID int64) (string, string) {
		// Identical name across calls is the whole point.
		return "shared-runner", "shared-runner"
	}

	done := make(chan struct{}, 2)
	for range 2 {
		go func() { s.dispatch(context.Background(), 1); done <- struct{}{} }()
	}
	<-done
	<-done

	if got := ln.calls.Load(); got != 1 {
		t.Fatalf("Launch calls = %d, want exactly 1 under concurrent dispatch of same jobID", got)
	}
	active, _ := st.ListActiveRunners(context.Background())
	if len(active) != 1 {
		t.Fatalf("active runners = %d, want 1", len(active))
	}
}

// errStore is a minimal store.Store stub used to inject failures the real
// SQLite store can't easily produce.
type errStore struct {
	pendingErr error
}

func (e *errStore) PendingJobs(ctx context.Context) ([]*store.Job, error) {
	return nil, e.pendingErr
}

// All other Store methods are unused by the tests that touch errStore;
// they panic if exercised so an accidental call surfaces loudly.
func (e *errStore) SaveAppConfig(context.Context, *store.AppConfig) error { panic("unused") }
func (e *errStore) GetAppConfig(context.Context) (*store.AppConfig, error) { panic("unused") }
func (e *errStore) UpsertInstallation(context.Context, *store.Installation) error { panic("unused") }
func (e *errStore) ListInstallations(context.Context) ([]*store.Installation, error) { panic("unused") }
func (e *errStore) UpsertRepoInstallation(context.Context, string, int64) error { panic("unused") }
func (e *errStore) RemoveRepoInstallation(context.Context, string) error { panic("unused") }
func (e *errStore) InstallationForRepo(context.Context, string) (*store.Installation, error) {
	panic("unused")
}
func (e *errStore) InsertJobIfNew(context.Context, *store.Job) (bool, error) { panic("unused") }
func (e *errStore) GetJob(context.Context, int64) (*store.Job, error)        { panic("unused") }
func (e *errStore) MarkJobDispatched(context.Context, int64) error            { panic("unused") }
func (e *errStore) MarkJobInProgress(context.Context, int64, int64, string) error {
	panic("unused")
}
func (e *errStore) MarkJobCompleted(context.Context, int64, string) error { panic("unused") }
func (e *errStore) InsertRunner(context.Context, *store.Runner) error     { panic("unused") }
func (e *errStore) UpdateRunnerStatus(context.Context, string, string) error {
	panic("unused")
}
func (e *errStore) UpdateRunnerStatusByName(context.Context, string, string) error {
	panic("unused")
}
func (e *errStore) ActiveRunnerCount(context.Context) (int, error)      { panic("unused") }
func (e *errStore) ListActiveRunners(context.Context) ([]*store.Runner, error) { panic("unused") }
func (e *errStore) Close() error                                        { return nil }
