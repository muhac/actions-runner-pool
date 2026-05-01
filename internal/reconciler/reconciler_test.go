package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/muhac/actions-runner-pool/internal/github"
	"github.com/muhac/actions-runner-pool/internal/store"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeStore struct {
	mu            sync.Mutex
	rows          []*store.Runner
	activeByCall  [][]*store.Runner
	activeCalls   int
	updates       []update
	listErr       error
	updErr        error
	appCfg        *store.AppConfig
	installations map[string]*store.Installation
	// jobs + jobMarks back the stale-in_progress sweep tests. Tests
	// that don't exercise that sweep can leave both nil.
	jobs        []*store.Job
	listJobsErr error
	jobMarks    []jobMark
	markErr     error
}

type jobMark struct {
	id         int64
	conclusion string
}

type update struct {
	container, status string
}

func (f *fakeStore) ListActiveRunners(ctx context.Context) ([]*store.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activeCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.activeByCall) > 0 {
		idx := f.activeCalls - 1
		if idx >= len(f.activeByCall) {
			idx = len(f.activeByCall) - 1
		}
		out := make([]*store.Runner, len(f.activeByCall[idx]))
		copy(out, f.activeByCall[idx])
		return out, nil
	}
	out := make([]*store.Runner, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

func (f *fakeStore) UpdateRunnerStatus(ctx context.Context, container, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updErr != nil {
		return f.updErr
	}
	f.updates = append(f.updates, update{container, status})
	// Reflect transition in rows so subsequent ListActiveRunners doesn't
	// keep returning a finished row (mirrors real store behavior).
	if status == "finished" {
		filtered := f.rows[:0]
		for _, r := range f.rows {
			if r.ContainerName != container {
				filtered = append(filtered, r)
			}
		}
		f.rows = filtered
	}
	return nil
}

// GetAppConfig + InstallationForRepo: only the GitHub-side sweep
// touches these. The fake returns nil to make any accidental call
// surface as a clean "no app config / no installation" log line in
// tests that don't intentionally exercise the GitHub sweep.
func (f *fakeStore) GetAppConfig(ctx context.Context) (*store.AppConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appCfg, nil
}

func (f *fakeStore) InstallationForRepo(ctx context.Context, repo string) (*store.Installation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installations == nil {
		return nil, nil
	}
	return f.installations[repo], nil
}

func (f *fakeStore) ListJobs(ctx context.Context, _ store.JobListFilter) ([]*store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listJobsErr != nil {
		return nil, f.listJobsErr
	}
	out := make([]*store.Job, len(f.jobs))
	copy(out, f.jobs)
	return out, nil
}

func (f *fakeStore) MarkJobCompleted(ctx context.Context, jobID int64, conclusion string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return false, f.markErr
	}
	f.jobMarks = append(f.jobMarks, jobMark{id: jobID, conclusion: conclusion})
	for _, j := range f.jobs {
		if j.ID == jobID {
			j.Status = "completed"
			j.Conclusion = conclusion
			return true, nil
		}
	}
	return false, nil
}

// ListAllInstallationRepos: tests opt in by setting f.installations
// (the same map InstallationForRepo reads). Iteration order matters
// for some assertions, so sort by repo name.
func (f *fakeStore) ListAllInstallationRepos(ctx context.Context) ([]store.RepoInstallation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	repos := make([]string, 0, len(f.installations))
	for r := range f.installations {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	out := make([]store.RepoInstallation, 0, len(repos))
	for _, r := range repos {
		inst := f.installations[r]
		if inst == nil {
			continue
		}
		out = append(out, store.RepoInstallation{Repo: r, InstallationID: inst.ID})
	}
	return out, nil
}

type fakeDocker struct {
	mu           sync.Mutex
	exists       map[string]bool
	prefixList   []ContainerInfo
	removed      []string
	inspectCalls int
	inspectErr   error
	listErr      error
	removeErr    error
}

func (f *fakeDocker) Inspect(ctx context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return false, f.inspectErr
	}
	f.inspectCalls++
	return f.exists[name], nil
}

func (f *fakeDocker) ListByPrefix(ctx context.Context, prefix string) ([]ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]ContainerInfo, len(f.prefixList))
	copy(out, f.prefixList)
	return out, nil
}

func (f *fakeDocker) ForceRemove(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, name)
	return nil
}

func newRecon(st Store, dk Docker) *Reconciler {
	r := New(st, dk, nil, quietLog(), 1*time.Hour, "gharp-", "", nil, 0)
	r.period = 10 * time.Millisecond
	r.grace = 5 * time.Minute
	r.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return r
}

// (1) DB row exists, container gone → mark finished. The "cap deadlock"
// fix path: this is exactly what unblocks the queue.
func TestReconcile_GhostRunner_MarksFinished(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		{ContainerName: "gharp-1-aaaa", RunnerName: "gharp-1-aaaa", Status: "idle", StartedAt: time.Unix(1_699_990_000, 0)},
	}}
	dk := &fakeDocker{exists: map[string]bool{}}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(st.updates) != 1 || st.updates[0] != (update{"gharp-1-aaaa", "finished"}) {
		t.Fatalf("expected single finished update, got %+v", st.updates)
	}
}

// (1b) Container still exists → row left alone.
func TestReconcile_LiveRunner_NoChange(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		// 60s before nowFn — well under the 1h test maxLifetime.
		{ContainerName: "gharp-2-bbbb", Status: "busy", StartedAt: time.Unix(1_700_000_000-60, 0)},
	}}
	dk := &fakeDocker{exists: map[string]bool{"gharp-2-bbbb": true}}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(st.updates) != 0 {
		t.Fatalf("expected no updates, got %+v", st.updates)
	}
}

// (1c) Inspect error → conservative no-op. Don't mark live runners
// finished just because the docker socket hiccuped.
func TestReconcile_InspectError_DoesNotMarkFinished(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		{ContainerName: "gharp-3-cccc", Status: "idle", StartedAt: time.Unix(1_699_990_000, 0)},
	}}
	dk := &fakeDocker{exists: map[string]bool{}, inspectErr: errors.New("docker daemon down")}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(st.updates) != 0 {
		t.Fatalf("expected no updates on inspect error, got %+v", st.updates)
	}
}

// (4) Lifetime timeout: container is alive but the row's StartedAt
// is past maxLifetime → docker rm -f + mark finished. Defends against
// EPHEMERAL runners that registered but never claimed a job.
func TestReconcile_LifetimeTimeout_ForceRemovesAndMarksFinished(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		// StartedAt 2h before now. With newRecon's 1h maxLifetime,
		// this row is past the cap.
		{ContainerName: "gharp-stuck-aaaa", Status: "starting", StartedAt: time.Unix(1_700_000_000-7200, 0)},
	}}
	dk := &fakeDocker{exists: map[string]bool{"gharp-stuck-aaaa": true}}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 1 || dk.removed[0] != "gharp-stuck-aaaa" {
		t.Fatalf("expected ForceRemove of stuck runner, got %+v", dk.removed)
	}
	if len(st.updates) != 1 || st.updates[0] != (update{"gharp-stuck-aaaa", "finished"}) {
		t.Fatalf("expected finished update after lifetime reap, got %+v", st.updates)
	}
}

// (4b) Within lifetime: container alive, row young → no action.
func TestReconcile_WithinLifetime_NoAction(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		// 5 minutes old. Well under 1h.
		{ContainerName: "gharp-young-bbbb", Status: "busy", StartedAt: time.Unix(1_700_000_000-300, 0)},
	}}
	dk := &fakeDocker{exists: map[string]bool{"gharp-young-bbbb": true}}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 0 {
		t.Fatalf("young runner removed: %+v", dk.removed)
	}
	if len(st.updates) != 0 {
		t.Fatalf("young runner status updated: %+v", st.updates)
	}
}

// (4c) ForceRemove failure path: must NOT mark the row finished while
// the container could still be running, otherwise the cap slot would
// free up and a stuck container could double-claim jobs.
func TestReconcile_LifetimeTimeout_ForceRemoveFailure_KeepsRow(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		{ContainerName: "gharp-rmerr-cccc", Status: "starting", StartedAt: time.Unix(1_700_000_000-7200, 0)},
	}}
	dk := &fakeDocker{
		exists:    map[string]bool{"gharp-rmerr-cccc": true},
		removeErr: errors.New("docker rm: socket eof"),
	}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(st.updates) != 0 {
		t.Fatalf("row marked finished despite ForceRemove failure: %+v", st.updates)
	}
}

// (3) Container running, no row → orphan, force removed (when old enough).
func TestReconcile_OrphanContainer_ForceRemoved(t *testing.T) {
	st := &fakeStore{} // no active runners
	dk := &fakeDocker{
		exists: map[string]bool{},
		// CreatedAt 10 minutes before now → past the 5m grace.
		prefixList: []ContainerInfo{{Name: "gharp-99-zzzz", CreatedAt: time.Unix(1_700_000_000-600, 0)}},
	}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 1 || dk.removed[0] != "gharp-99-zzzz" {
		t.Fatalf("expected ForceRemove of gharp-99-zzzz, got %+v", dk.removed)
	}
}

// (3b) Per-container grace: a young orphan is deferred regardless of
// what other rows look like.
func TestReconcile_OrphanGrace_DefersYoungContainer(t *testing.T) {
	st := &fakeStore{}
	dk := &fakeDocker{
		exists: map[string]bool{},
		// CreatedAt 10s before now → inside the 5m grace.
		prefixList: []ContainerInfo{{Name: "gharp-11-eeee", CreatedAt: time.Unix(1_700_000_000-10, 0)}},
	}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 0 {
		t.Fatalf("young orphan removed during grace window: %+v", dk.removed)
	}
}

// (3c) Activity-host case: a steady stream of young active rows must
// NOT defer cleanup of an actually-old orphan. Earlier implementations
// gated the entire sweep on "any active row younger than grace" and
// would leak orphans indefinitely under continuous load.
func TestReconcile_OrphanGrace_PerContainer_NotPerHost(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		// New dispatch landed 10s ago — would have blocked the old
		// host-wide-grace logic.
		{ContainerName: "gharp-fresh-aaaa", Status: "starting", StartedAt: time.Unix(1_700_000_000-10, 0)},
	}}
	dk := &fakeDocker{
		exists: map[string]bool{"gharp-fresh-aaaa": true},
		prefixList: []ContainerInfo{
			{Name: "gharp-fresh-aaaa", CreatedAt: time.Unix(1_700_000_000-10, 0)},   // young, claimed
			{Name: "gharp-stale-bbbb", CreatedAt: time.Unix(1_700_000_000-3600, 0)}, // 1h old, orphan
		},
	}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 1 || dk.removed[0] != "gharp-stale-bbbb" {
		t.Fatalf("expected only the old orphan removed, got %+v", dk.removed)
	}
}

// (3d) CreatedAt zero (parse failure upstream) is treated as old —
// better to over-clean an undatable orphan than to leak it.
func TestReconcile_OrphanGrace_ZeroCreatedAtRemoved(t *testing.T) {
	st := &fakeStore{}
	dk := &fakeDocker{
		exists:     map[string]bool{},
		prefixList: []ContainerInfo{{Name: "gharp-undated-cccc"}}, // CreatedAt is zero
	}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 1 || dk.removed[0] != "gharp-undated-cccc" {
		t.Fatalf("expected ForceRemove of undated orphan, got %+v", dk.removed)
	}
}

// Run loop wiring: ctx cancel returns ctx.Err and stops ticking.
func TestRun_ContextCancel(t *testing.T) {
	st := &fakeStore{}
	dk := &fakeDocker{}
	r := newRecon(st, dk)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	time.Sleep(25 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRun_GitHubSweepDoesNotBlockDockerLoop(t *testing.T) {
	st := &fakeStore{
		rows: []*store.Runner{
			{ContainerName: "gharp-live", RunnerName: "gharp-live", Repo: "alice/repo", Status: "idle", StartedAt: time.Unix(1_700_000_000-60, 0)},
		},
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{"alice/repo": {ID: 99}},
	}
	dk := &fakeDocker{exists: map[string]bool{"gharp-live": true}}
	ghBlock := make(chan struct{})
	gh := &fakeGH{listBlock: ghBlock, runnersByRepo: map[string][]github.RepoRunner{"alice/repo": nil}}
	r := New(st, dk, gh, quietLog(), 1*time.Hour, "gharp-", "", nil, 0)
	r.period = 10 * time.Millisecond
	r.githubSweepPeriod = time.Hour
	r.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() {
		close(ghBlock)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}()

	deadline := time.After(time.Second)
	for {
		gh.mu.Lock()
		blocked := gh.listCalls > 0
		gh.mu.Unlock()
		if blocked {
			break
		}
		select {
		case <-deadline:
			t.Fatal("GitHub sweep did not reach ListRepoRunners")
		case <-time.After(time.Millisecond):
		}
	}

	deadline = time.After(time.Second)
	for {
		dk.mu.Lock()
		inspectCalls := dk.inspectCalls
		dk.mu.Unlock()
		if inspectCalls >= 3 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("docker loop blocked behind GitHub sweep; inspect calls=%d", inspectCalls)
		case <-time.After(time.Millisecond):
		}
	}
}

// --- GitHub-side ghost sweep --------------------------------------

type fakeGH struct {
	mu             sync.Mutex
	runnersByRepo  map[string][]github.RepoRunner
	deleted        []deletedRunner
	listBlock      chan struct{}
	listErr        error
	deleteErr      error
	jwtErr         error
	instTokenErr   error
	jwtCalls       int
	instTokenCalls int
	listCalls      int
	deleteCalls    int
	// jobsByID + jobErr + jobCalls back the WorkflowJob fake used by
	// the stale-in_progress sweep tests. Tests not exercising that
	// sweep can leave them nil/zero.
	jobsByID map[int64]*github.WorkflowJobStatus
	jobErr   error
	jobCalls int
}

type deletedRunner struct {
	repo string
	id   int64
}

func (g *fakeGH) AppJWT(_ []byte, _ int64) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.jwtCalls++
	if g.jwtErr != nil {
		return "", g.jwtErr
	}
	return "jwt", nil
}
func (g *fakeGH) InstallationToken(_ context.Context, _ string, _ int64) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.instTokenCalls++
	if g.instTokenErr != nil {
		return "", g.instTokenErr
	}
	return "inst", nil
}
func (g *fakeGH) ListRepoRunners(_ context.Context, _ string, repo string) ([]github.RepoRunner, error) {
	g.mu.Lock()
	g.listCalls++
	if g.listErr != nil {
		g.mu.Unlock()
		return nil, g.listErr
	}
	g.mu.Unlock()
	if g.listBlock != nil {
		<-g.listBlock
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.runnersByRepo[repo], nil
}
func (g *fakeGH) DeleteRepoRunner(_ context.Context, _ string, repo string, id int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deleteCalls++
	if g.deleteErr != nil {
		return g.deleteErr
	}
	g.deleted = append(g.deleted, deletedRunner{repo, id})
	return nil
}

func (g *fakeGH) WorkflowJob(_ context.Context, _ string, _ string, jobID int64) (*github.WorkflowJobStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.jobCalls++
	if g.jobErr != nil {
		return nil, g.jobErr
	}
	if s, ok := g.jobsByID[jobID]; ok {
		return s, nil
	}
	// Default to "still in_progress" so tests that don't seed a row
	// observe the no-op path rather than a synthetic completion.
	return &github.WorkflowJobStatus{Status: "in_progress"}, nil
}

// (4) GitHub has runners that we no longer have rows for -> DELETE.
// Runner #1 is ours (matches an active row by name) -> preserved.
// Runner #2 is in our prefix namespace but not in the table -> DELETE.
// Runner #3 has the wrong prefix entirely -> preserved (not ours).
// Runner #4 is busy -> preserved (don't interrupt someone's job even
// if we can't account for it).
func TestReconcile_GitHubGhostSweep_DeletesUnknownRunners(t *testing.T) {
	st := &fakeStore{
		rows: []*store.Runner{
			{ContainerName: "gharp-1-aaaa", RunnerName: "gharp-1-aaaa", Repo: "alice/repo", Status: "busy", StartedAt: time.Unix(1_700_000_000-60, 0)},
		},
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{"alice/repo": {ID: 99}},
	}
	dk := &fakeDocker{exists: map[string]bool{"gharp-1-aaaa": true}}
	gh := &fakeGH{runnersByRepo: map[string][]github.RepoRunner{
		"alice/repo": {
			{ID: 11, Name: "gharp-1-aaaa", Status: "online", Busy: true},
			{ID: 12, Name: "gharp-9-zzzz", Status: "online", Busy: false},
			{ID: 13, Name: "other-system-runner", Status: "online", Busy: false},
			{ID: 14, Name: "gharp-7-busy", Status: "online", Busy: true},
		},
	}}
	r := New(st, dk, gh, quietLog(), 1*time.Hour, "gharp-", "", nil, 0)
	r.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	r.sweepGitHubGhostRunners(context.Background())

	if len(gh.deleted) != 1 {
		t.Fatalf("expected 1 DELETE, got %+v", gh.deleted)
	}
	if gh.deleted[0] != (deletedRunner{"alice/repo", 12}) {
		t.Fatalf("wrong runner deleted: %+v", gh.deleted[0])
	}
}

// No installed repos at all (no app, no installations) -> no work,
// no token mint. The sweep short-circuits cheaply when there's
// literally nothing to inspect.
func TestReconcile_GitHubGhostSweep_NoInstalledRepos_NoOp(t *testing.T) {
	st := &fakeStore{appCfg: &store.AppConfig{AppID: 1, PEM: []byte("pem")}}
	dk := &fakeDocker{}
	gh := &fakeGH{}
	r := New(st, dk, gh, quietLog(), 1*time.Hour, "gharp-", "", nil, 0)
	r.sweepGitHubGhostRunners(context.Background())
	if gh.jwtCalls != 0 || gh.instTokenCalls != 0 || gh.listCalls != 0 {
		t.Fatalf("idle deployment burned API: jwt=%d inst=%d list=%d",
			gh.jwtCalls, gh.instTokenCalls, gh.listCalls)
	}
}

// Repo has zero active rows but is in installation_repos -> the sweep
// must still query GitHub for that repo and clear any prefixed ghost
// runners. This is the "deployment goes idle, then GitHub-side
// ghosts pile up" failure mode.
func TestReconcile_GitHubGhostSweep_IdleRepoWithGhost_Cleared(t *testing.T) {
	st := &fakeStore{
		// No active rows.
		rows:          nil,
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{"alice/repo": {ID: 99}},
	}
	dk := &fakeDocker{}
	gh := &fakeGH{runnersByRepo: map[string][]github.RepoRunner{
		"alice/repo": {
			{ID: 42, Name: "gharp-stale-deadbeef", Status: "offline", Busy: false},
			{ID: 43, Name: "other-system-runner", Status: "online", Busy: false},
		},
	}}
	r := New(st, dk, gh, quietLog(), 1*time.Hour, "gharp-", "", nil, 0)
	r.sweepGitHubGhostRunners(context.Background())
	if len(gh.deleted) != 1 || gh.deleted[0] != (deletedRunner{"alice/repo", 42}) {
		t.Fatalf("expected DELETE of gharp-stale-deadbeef from alice/repo, got %+v", gh.deleted)
	}
}

func TestReconcile_GitHubGhostSweep_RechecksActiveRunnerBeforeDelete(t *testing.T) {
	st := &fakeStore{
		activeByCall: [][]*store.Runner{
			nil,
			{
				{ContainerName: "gharp-new-runner", RunnerName: "gharp-new-runner", Repo: "alice/repo", Status: "starting", StartedAt: time.Unix(1_700_000_000, 0)},
			},
		},
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{"alice/repo": {ID: 99}},
	}
	dk := &fakeDocker{}
	gh := &fakeGH{runnersByRepo: map[string][]github.RepoRunner{
		"alice/repo": {
			{ID: 42, Name: "gharp-new-runner", Status: "online", Busy: false},
		},
	}}
	r := New(st, dk, gh, quietLog(), 1*time.Hour, "gharp-", "", nil, 0)
	r.sweepGitHubGhostRunners(context.Background())
	if len(gh.deleted) != 0 {
		t.Fatalf("new active runner deleted after recheck: %+v", gh.deleted)
	}
	if st.activeCalls < 2 {
		t.Fatalf("expected active runner recheck before delete, got %d calls", st.activeCalls)
	}
}

// Multiple repos under one installation share a single install
// token: 2 repos -> 1 InstallationToken call, 2 ListRepoRunners
// calls. Matters because tokens cost rate limit at /app/.../tokens.
func TestReconcile_GitHubGhostSweep_TokenCachedPerInstallation(t *testing.T) {
	st := &fakeStore{
		rows: []*store.Runner{
			{ContainerName: "gharp-1-a", RunnerName: "gharp-1-a", Repo: "alice/r1", Status: "busy", StartedAt: time.Unix(1_700_000_000-60, 0)},
			{ContainerName: "gharp-2-a", RunnerName: "gharp-2-a", Repo: "alice/r2", Status: "busy", StartedAt: time.Unix(1_700_000_000-60, 0)},
		},
		appCfg: &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{
			"alice/r1": {ID: 99},
			"alice/r2": {ID: 99}, // same install
		},
	}
	dk := &fakeDocker{exists: map[string]bool{"gharp-1-a": true, "gharp-2-a": true}}
	gh := &fakeGH{runnersByRepo: map[string][]github.RepoRunner{
		"alice/r1": {{ID: 11, Name: "gharp-1-a", Status: "online"}},
		"alice/r2": {{ID: 12, Name: "gharp-2-a", Status: "online"}},
	}}
	r := New(st, dk, gh, quietLog(), 1*time.Hour, "gharp-", "", nil, 0)
	r.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	r.sweepGitHubGhostRunners(context.Background())
	if gh.instTokenCalls != 1 {
		t.Fatalf("expected 1 InstallationToken (cached), got %d", gh.instTokenCalls)
	}
	if gh.listCalls != 2 {
		t.Fatalf("expected 2 ListRepoRunners (one per repo), got %d", gh.listCalls)
	}
}

// nil GitHubClient disables the sweep entirely; constructing with
// nil + calling Reconcile + Run must not panic.
func TestReconcile_GitHubGhostSweep_NilGH_Disabled(t *testing.T) {
	st := &fakeStore{}
	dk := &fakeDocker{}
	r := New(st, dk, nil, quietLog(), 1*time.Hour, "gharp-", "", nil, 0)
	r.sweepGitHubGhostRunners(context.Background()) // no panic
}

// --- Stale in_progress sweep --------------------------------------

// staleInProgressNow + staleInProgressLifetime are the standard
// fixture for these tests: lifetime 1h, "now" anchored at a stable
// epoch, and the helper jobUpdated(d) returning a job whose
// updated_at is `d` ago.
const (
	staleInProgressNowEpoch  = int64(1_700_000_000)
	staleInProgressLifetime  = 1 * time.Hour
	staleInProgressTestRepo  = "alice/repo"
	staleInProgressOtherRepo = "alice/other"
)

func staleInProgressNow() time.Time { return time.Unix(staleInProgressNowEpoch, 0) }

func newStaleSweepReconciler(st Store, gh GitHubClient) *Reconciler {
	r := New(st, &fakeDocker{}, gh, quietLog(), staleInProgressLifetime, "gharp-", "", nil, 0)
	r.nowFn = staleInProgressNow
	return r
}

func staleJob(id int64, repo string, ageBeforeNow time.Duration) *store.Job {
	return &store.Job{
		ID:        id,
		Repo:      repo,
		Status:    "in_progress",
		UpdatedAt: staleInProgressNow().Add(-ageBeforeNow),
	}
}

// Stale in_progress + GitHub says completed → MarkJobCompleted with
// the GH-side conclusion. The core happy path: webhook completion
// was lost, the sweep restores ground truth.
func TestStaleInProgressSweep_CompletedOnGitHub_MarksRow(t *testing.T) {
	st := &fakeStore{
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{staleInProgressTestRepo: {ID: 99}},
		jobs:          []*store.Job{staleJob(123, staleInProgressTestRepo, 2*time.Hour)},
	}
	gh := &fakeGH{jobsByID: map[int64]*github.WorkflowJobStatus{
		123: {Status: "completed", Conclusion: "success"},
	}}
	r := newStaleSweepReconciler(st, gh)
	r.sweepStaleInProgressJobs(context.Background())
	if len(st.jobMarks) != 1 || st.jobMarks[0] != (jobMark{id: 123, conclusion: "success"}) {
		t.Fatalf("expected MarkJobCompleted(123,success), got %+v", st.jobMarks)
	}
	if gh.jobCalls != 1 {
		t.Fatalf("expected exactly 1 WorkflowJob call, got %d", gh.jobCalls)
	}
}

// Stale in_progress + GitHub still says in_progress → no write.
// Webhook isn't lost, the job is genuinely still running.
func TestStaleInProgressSweep_StillRunningOnGitHub_NoOp(t *testing.T) {
	st := &fakeStore{
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{staleInProgressTestRepo: {ID: 99}},
		jobs:          []*store.Job{staleJob(7, staleInProgressTestRepo, 3*time.Hour)},
	}
	gh := &fakeGH{jobsByID: map[int64]*github.WorkflowJobStatus{
		7: {Status: "in_progress"},
	}}
	r := newStaleSweepReconciler(st, gh)
	r.sweepStaleInProgressJobs(context.Background())
	if len(st.jobMarks) != 0 {
		t.Fatalf("expected no MarkJobCompleted, got %+v", st.jobMarks)
	}
}

// AuthFailed (401/403) means the App was almost certainly
// uninstalled. The webhook for completion can no longer arrive
// either; leaving the row in_progress would burn a WorkflowJob call
// every sweep cycle forever. Treat as terminal/cancelled, same
// policy as NotFound.
func TestStaleInProgressSweep_AuthFailed_MarksCancelled(t *testing.T) {
	st := &fakeStore{
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{staleInProgressTestRepo: {ID: 99}},
		jobs:          []*store.Job{staleJob(401, staleInProgressTestRepo, 2*time.Hour)},
	}
	gh := &fakeGH{jobsByID: map[int64]*github.WorkflowJobStatus{
		401: {AuthFailed: true},
	}}
	r := newStaleSweepReconciler(st, gh)
	r.sweepStaleInProgressJobs(context.Background())
	if len(st.jobMarks) != 1 || st.jobMarks[0] != (jobMark{id: 401, conclusion: "cancelled"}) {
		t.Fatalf("expected MarkJobCompleted(401,cancelled), got %+v", st.jobMarks)
	}
}

// 404 from GitHub → mark cancelled (mirrors dispatch's NotFound
// policy).
func TestStaleInProgressSweep_NotFound_MarksCancelled(t *testing.T) {
	st := &fakeStore{
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{staleInProgressTestRepo: {ID: 99}},
		jobs:          []*store.Job{staleJob(404, staleInProgressTestRepo, 2*time.Hour)},
	}
	gh := &fakeGH{jobsByID: map[int64]*github.WorkflowJobStatus{
		404: {NotFound: true},
	}}
	r := newStaleSweepReconciler(st, gh)
	r.sweepStaleInProgressJobs(context.Background())
	if len(st.jobMarks) != 1 || st.jobMarks[0] != (jobMark{id: 404, conclusion: "cancelled"}) {
		t.Fatalf("expected MarkJobCompleted(404,cancelled), got %+v", st.jobMarks)
	}
}

// Fresh in_progress (within maxLifetime) → no GH call. This is the
// hot path: in steady state every in_progress row is fresh and the
// sweep must short-circuit before burning install-token quota.
func TestStaleInProgressSweep_FreshRow_NoGitHubCall(t *testing.T) {
	st := &fakeStore{
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{staleInProgressTestRepo: {ID: 99}},
		jobs:          []*store.Job{staleJob(1, staleInProgressTestRepo, 5*time.Minute)},
	}
	gh := &fakeGH{}
	r := newStaleSweepReconciler(st, gh)
	r.sweepStaleInProgressJobs(context.Background())
	if gh.jwtCalls != 0 || gh.instTokenCalls != 0 || gh.jobCalls != 0 {
		t.Fatalf("fresh row burned GH API: jwt=%d inst=%d job=%d", gh.jwtCalls, gh.instTokenCalls, gh.jobCalls)
	}
	if len(st.jobMarks) != 0 {
		t.Fatalf("expected no MarkJobCompleted on fresh row, got %+v", st.jobMarks)
	}
}

// nil gh disables the sweep entirely (parity with the
// ghost-runner sweep).
func TestStaleInProgressSweep_NilGH_Disabled(t *testing.T) {
	st := &fakeStore{
		jobs: []*store.Job{staleJob(1, staleInProgressTestRepo, 2*time.Hour)},
	}
	r := New(st, &fakeDocker{}, nil, quietLog(), staleInProgressLifetime, "gharp-", "", nil, 0)
	r.nowFn = staleInProgressNow
	r.sweepStaleInProgressJobs(context.Background())
	if len(st.jobMarks) != 0 {
		t.Fatalf("nil gh should disable sweep, got marks %+v", st.jobMarks)
	}
}

// maxLifetime == 0 disables the sweep (paranoia config; same
// rationale as the runner-side lifetime gate at reconciler.go:296).
func TestStaleInProgressSweep_ZeroLifetime_Disabled(t *testing.T) {
	st := &fakeStore{
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{staleInProgressTestRepo: {ID: 99}},
		jobs:          []*store.Job{staleJob(1, staleInProgressTestRepo, 24*time.Hour)},
	}
	gh := &fakeGH{jobsByID: map[int64]*github.WorkflowJobStatus{
		1: {Status: "completed", Conclusion: "success"},
	}}
	r := New(st, &fakeDocker{}, gh, quietLog(), 0, "gharp-", "", nil, 0)
	r.nowFn = staleInProgressNow
	r.sweepStaleInProgressJobs(context.Background())
	if gh.jobCalls != 0 || len(st.jobMarks) != 0 {
		t.Fatalf("zero lifetime should disable sweep, got jobCalls=%d marks=%+v", gh.jobCalls, st.jobMarks)
	}
}

// Multiple stale rows under one installation share a single install
// token. Mirrors the existing TokenCachedPerInstallation test for
// the ghost-runner sweep — same quota concern.
func TestStaleInProgressSweep_TokenCachedPerInstallation(t *testing.T) {
	st := &fakeStore{
		appCfg: &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{
			staleInProgressTestRepo:  {ID: 99},
			staleInProgressOtherRepo: {ID: 99}, // same installation
		},
		jobs: []*store.Job{
			staleJob(1, staleInProgressTestRepo, 2*time.Hour),
			staleJob(2, staleInProgressOtherRepo, 2*time.Hour),
		},
	}
	gh := &fakeGH{jobsByID: map[int64]*github.WorkflowJobStatus{
		1: {Status: "completed", Conclusion: "success"},
		2: {Status: "completed", Conclusion: "failure"},
	}}
	r := newStaleSweepReconciler(st, gh)
	r.sweepStaleInProgressJobs(context.Background())
	if gh.instTokenCalls != 1 {
		t.Fatalf("expected 1 InstallationToken (cached), got %d", gh.instTokenCalls)
	}
	if gh.jobCalls != 2 {
		t.Fatalf("expected 2 WorkflowJob (one per stale row), got %d", gh.jobCalls)
	}
	if len(st.jobMarks) != 2 {
		t.Fatalf("expected 2 marks, got %+v", st.jobMarks)
	}
}

// A failed InstallationToken mint must not be retried for every
// other stale job in the same installation. With N jobs in one
// installation and a flaky token endpoint, this would otherwise
// cost N mint attempts per sweep.
func TestStaleInProgressSweep_TokenMintFailure_NotRetriedPerJob(t *testing.T) {
	st := &fakeStore{
		appCfg: &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{
			staleInProgressTestRepo:  {ID: 99},
			staleInProgressOtherRepo: {ID: 99}, // same installation
		},
		jobs: []*store.Job{
			staleJob(1, staleInProgressTestRepo, 2*time.Hour),
			staleJob(2, staleInProgressOtherRepo, 2*time.Hour),
			staleJob(3, staleInProgressTestRepo, 2*time.Hour),
		},
	}
	gh := &fakeGH{instTokenErr: errors.New("boom")}
	r := newStaleSweepReconciler(st, gh)
	r.sweepStaleInProgressJobs(context.Background())
	if gh.instTokenCalls != 1 {
		t.Fatalf("expected exactly 1 InstallationToken attempt, got %d", gh.instTokenCalls)
	}
	if gh.jobCalls != 0 {
		t.Fatalf("WorkflowJob should not be called when token mint failed, got %d", gh.jobCalls)
	}
	if len(st.jobMarks) != 0 {
		t.Fatalf("no marks expected without a valid token, got %+v", st.jobMarks)
	}
}

// Stale row whose repo isn't in the installations map is left alone
// (this happens after an installation is removed but the row hasn't
// been swept by the cancel-pending-jobs path yet — webhook for the
// cancellation could have been the one that got lost). We don't try
// to mint a token without an installation; we log and skip.
func TestStaleInProgressSweep_RepoWithoutInstallation_Skipped(t *testing.T) {
	st := &fakeStore{
		appCfg:        &store.AppConfig{AppID: 1, PEM: []byte("pem")},
		installations: map[string]*store.Installation{}, // empty
		jobs:          []*store.Job{staleJob(1, staleInProgressTestRepo, 2*time.Hour)},
	}
	gh := &fakeGH{}
	r := newStaleSweepReconciler(st, gh)
	r.sweepStaleInProgressJobs(context.Background())
	if gh.jobCalls != 0 || len(st.jobMarks) != 0 {
		t.Fatalf("missing installation should skip GH call and mark, got jobCalls=%d marks=%+v", gh.jobCalls, st.jobMarks)
	}
}

func TestReconcile_GhostRunner_CleansWorkdir(t *testing.T) {
	root := t.TempDir()
	container := "gharp-1-aaaa"
	if err := os.MkdirAll(filepath.Join(root, container, "_work"), 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	st := &fakeStore{rows: []*store.Runner{{
		ContainerName: container, RunnerName: container, Status: "idle", StartedAt: time.Unix(1_699_990_000, 0),
	}}}
	dk := &fakeDocker{exists: map[string]bool{}}
	r := New(st, dk, nil, quietLog(), 1*time.Hour, "gharp-", root, nil, 0)
	r.nowFn = func() time.Time { return time.Unix(1_700_000_000, 0) }
	r.Reconcile(context.Background())
	if _, err := os.Stat(filepath.Join(root, container)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected workdir removed, stat err=%v", err)
	}
}

func TestSweepOrphanWorkdirs_RemovesOnlyStaleUnclaimedDirs(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	stale := filepath.Join(root, "gharp-stale-aaaa")
	live := filepath.Join(root, "gharp-live-bbbb")
	young := filepath.Join(root, "gharp-young-cccc")
	other := filepath.Join(root, "other-prefix")
	for _, d := range []string{stale, live, young, other} {
		if err := os.MkdirAll(filepath.Join(d, "_work"), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	old := now.Add(-10 * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}
	if err := os.Chtimes(live, old, old); err != nil {
		t.Fatalf("chtimes live: %v", err)
	}
	if err := os.Chtimes(young, now.Add(-10*time.Second), now.Add(-10*time.Second)); err != nil {
		t.Fatalf("chtimes young: %v", err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatalf("chtimes other: %v", err)
	}

	dk := &fakeDocker{exists: map[string]bool{"gharp-live-bbbb": true}}
	r := New(&fakeStore{}, dk, nil, quietLog(), 1*time.Hour, "gharp-", root, nil, 0)
	r.nowFn = func() time.Time { return now }
	r.workdirGrace = 5 * time.Minute
	removed := r.sweepOrphanWorkdirs(context.Background())
	if removed != 1 {
		t.Fatalf("expected 1 removed workdir, got %d", removed)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale dir should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live dir should remain: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("young dir should remain: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("other prefix dir should remain: %v", err)
	}
}

func TestRunMaintenanceCommand_Success(t *testing.T) {
	r := New(&fakeStore{}, &fakeDocker{exists: map[string]bool{}}, nil, quietLog(),
		1*time.Hour, "gharp-", "", []string{"true"}, time.Minute)
	// should not panic or return error
	r.runMaintenanceCommand(context.Background())
}

func TestRunMaintenanceCommand_Failure_NocrashService(t *testing.T) {
	r := New(&fakeStore{}, &fakeDocker{exists: map[string]bool{}}, nil, quietLog(),
		1*time.Hour, "gharp-", "", []string{"false"}, time.Minute)
	// non-zero exit must not panic
	r.runMaintenanceCommand(context.Background())
}

func TestRunMaintenanceCommand_Disabled_WhenNoCmd(t *testing.T) {
	r := New(&fakeStore{}, &fakeDocker{exists: map[string]bool{}}, nil, quietLog(),
		1*time.Hour, "gharp-", "", nil, time.Minute)
	// maintenanceCmd empty: goroutine never starts, method should be safe to call directly too
	r.runMaintenanceCommand(context.Background()) // should not panic (but it's a no-op when cmd is nil)
}
