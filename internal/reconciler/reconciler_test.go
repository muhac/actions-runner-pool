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
	prefixList  []string
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

func (f *fakeDocker) ListByPrefix(ctx context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]string, len(f.prefixList))
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

// (3) Container running, no row → orphan, force removed.
func TestReconcile_OrphanContainer_ForceRemoved(t *testing.T) {
	st := &fakeStore{} // no active runners
	dk := &fakeDocker{
		exists:     map[string]bool{},
		prefixList: []string{"gharp-99-zzzz"},
	}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 1 || dk.removed[0] != "gharp-99-zzzz" {
		t.Fatalf("expected ForceRemove of gharp-99-zzzz, got %+v", dk.removed)
	}
}

// (3b) Orphan sweep deferred while a young active runner row exists.
// Protects the gap between InsertRunner and the docker daemon ack.
func TestReconcile_OrphanGrace_DefersDuringYoungActive(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		// StartedAt 10s before now → inside the 5-minute grace.
		{ContainerName: "gharp-10-dddd", Status: "starting", StartedAt: time.Unix(1_700_000_000-10, 0)},
	}}
	dk := &fakeDocker{
		exists:     map[string]bool{"gharp-10-dddd": true},
		prefixList: []string{"gharp-10-dddd", "gharp-11-eeee"}, // 11 is the orphan
	}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 0 {
		t.Fatalf("orphan removed during grace window: %+v", dk.removed)
	}
}

// (3c) Once the youngest active runner ages past the grace window,
// the orphan sweep proceeds.
func TestReconcile_OrphanGrace_ProceedsAfterAged(t *testing.T) {
	st := &fakeStore{rows: []*store.Runner{
		// StartedAt 10 minutes before now → past the 5m grace.
		{ContainerName: "gharp-20-ffff", Status: "busy", StartedAt: time.Unix(1_700_000_000-600, 0)},
	}}
	dk := &fakeDocker{
		exists:     map[string]bool{"gharp-20-ffff": true},
		prefixList: []string{"gharp-20-ffff", "gharp-21-gggg"},
	}
	r := newRecon(st, dk)
	r.Reconcile(context.Background())
	if len(dk.removed) != 1 || dk.removed[0] != "gharp-21-gggg" {
		t.Fatalf("expected ForceRemove of orphan only, got %+v", dk.removed)
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
