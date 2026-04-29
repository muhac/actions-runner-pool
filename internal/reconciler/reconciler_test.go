package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/muhac/actions-runner-pool/internal/store"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeStore struct {
	mu      sync.Mutex
	rows    []*store.Runner
	updates []update
	listErr error
	updErr  error
}

type update struct {
	container, status string
}

func (f *fakeStore) ListActiveRunners(ctx context.Context) ([]*store.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
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

type fakeDocker struct {
	mu          sync.Mutex
	exists      map[string]bool
	prefixList  []ContainerInfo
	removed     []string
	inspectErr  error
	listErr     error
	removeErr   error
}

func (f *fakeDocker) Inspect(ctx context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return false, f.inspectErr
	}
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
	r := New(st, dk, quietLog())
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
		{ContainerName: "gharp-2-bbbb", Status: "busy", StartedAt: time.Unix(1_699_990_000, 0)},
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
