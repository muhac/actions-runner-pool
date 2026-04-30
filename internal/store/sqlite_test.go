package store

import (
	"context"
	"testing"
	"time"
)

func newStore(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite("file:" + t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSaveAndGetAppConfig(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if got, err := s.GetAppConfig(ctx); err != nil || got != nil {
		t.Fatalf("empty store: got=%v err=%v want nil,nil", got, err)
	}

	cfg := &AppConfig{
		AppID: 42, Slug: "gharp-test", WebhookSecret: "shh",
		PEM: []byte("-----PEM-----"), ClientID: "Iv1.x", ClientSecret: "sec",
		BaseURL: "https://example.com",
	}
	if err := s.SaveAppConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAppConfig(ctx)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.AppID != 42 || got.Slug != "gharp-test" || string(got.PEM) != "-----PEM-----" || got.BaseURL != "https://example.com" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	cfg.WebhookSecret = "new"
	if err := s.SaveAppConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAppConfig(ctx)
	if got.WebhookSecret != "new" {
		t.Fatalf("upsert did not overwrite: %s", got.WebhookSecret)
	}
}

func TestUpsertInstallation_Idempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	inst := &Installation{ID: 1, AccountID: 100, AccountLogin: "alice", AccountType: "User"}
	for range 2 {
		if err := s.UpsertInstallation(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListInstallations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].AccountLogin != "alice" {
		t.Fatalf("list = %v", list)
	}
}

func TestUpsertRepoInstallation_OverwritesOnReinstall(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, id := range []int64{1, 2} {
		if err := s.UpsertInstallation(ctx, &Installation{ID: id, AccountID: id, AccountLogin: "u", AccountType: "User"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertRepoInstallation(ctx, "alice/repo", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRepoInstallation(ctx, "alice/repo", 2); err != nil {
		t.Fatal(err)
	}
	got, err := s.InstallationForRepo(ctx, "alice/repo")
	if err != nil || got == nil {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if got.ID != 2 {
		t.Fatalf("InstallationForRepo.ID = %d, want 2", got.ID)
	}
}

func TestRemoveRepoInstallation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if err := s.UpsertInstallation(ctx, &Installation{ID: 1, AccountID: 1, AccountLogin: "u", AccountType: "User"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRepoInstallation(ctx, "alice/repo", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRepoInstallation(ctx, "alice/repo"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.InstallationForRepo(ctx, "alice/repo")
	if got != nil {
		t.Fatalf("after remove, got %+v", got)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// installation_id=999 has no parent row in installations.
	err := s.UpsertRepoInstallation(ctx, "alice/repo", 999)
	if err == nil {
		t.Fatalf("expected FK violation, got nil")
	}
}

func TestInsertJobIfNew_DupReturnsFalse(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := &Job{ID: 100, Repo: "a/b", Action: "queued", Labels: "self-hosted",
		DedupeKey: "100", Status: "pending"}
	first, err := s.InsertJobIfNew(ctx, j)
	if err != nil || !first {
		t.Fatalf("first: inserted=%v err=%v", first, err)
	}
	second, err := s.InsertJobIfNew(ctx, j)
	if err != nil {
		t.Fatalf("second err: %v", err)
	}
	if second {
		t.Fatalf("dup should return inserted=false")
	}
}

func TestMarkJobInProgressThenCompleted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := &Job{ID: 7, Repo: "a/b", Action: "queued", Labels: "x",
		DedupeKey: "7", Status: "pending"}
	if _, err := s.InsertJobIfNew(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkJobInProgress(ctx, 7, 99, "runner-7"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, 7)
	if got.Status != "in_progress" || got.RunnerID != 99 || got.RunnerName != "runner-7" {
		t.Fatalf("after in_progress: %+v", got)
	}
	completed, err := s.MarkJobCompleted(ctx, 7, "success")
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("MarkJobCompleted completed=false, want true")
	}
	got, _ = s.GetJob(ctx, 7)
	if got.Status != "completed" || got.Conclusion != "success" {
		t.Fatalf("after completed: %+v", got)
	}
}

func TestMarkJobDispatched_OnlyAdvancesPending(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := &Job{ID: 8, Repo: "a/b", Action: "queued", Labels: "x",
		DedupeKey: "8", Status: "pending"}
	if _, err := s.InsertJobIfNew(ctx, j); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkJobDispatched(ctx, 8); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, 8)
	if got.Status != "dispatched" {
		t.Fatalf("after dispatch: status=%s, want dispatched", got.Status)
	}
	if got.RunnerID != 0 || got.RunnerName != "" {
		t.Fatalf("dispatch must not touch binding: id=%d name=%q", got.RunnerID, got.RunnerName)
	}

	// Webhook lands later with the real binding — must promote dispatched → in_progress.
	if _, err := s.MarkJobInProgress(ctx, 8, 555, "runner-8"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, 8)
	if got.Status != "in_progress" {
		t.Fatalf("after in_progress: status=%s, want in_progress", got.Status)
	}
	if got.RunnerID != 555 || got.RunnerName != "runner-8" {
		t.Fatalf("real binding lost: %+v", got)
	}

	// Now: webhook arrived FIRST (real binding from 'pending'), dispatch's
	// MarkJobDispatched fires after — it must NOT clobber (status is
	// already 'in_progress', condition WHERE status='pending' filters out).
	j2 := &Job{ID: 9, Repo: "a/b", Action: "queued", Labels: "x",
		DedupeKey: "9", Status: "pending"}
	if _, err := s.InsertJobIfNew(ctx, j2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkJobInProgress(ctx, 9, 777, "runner-9"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJobDispatched(ctx, 9); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, 9)
	if got.Status != "in_progress" {
		t.Fatalf("MarkJobDispatched clobbered status: %s", got.Status)
	}
	if got.RunnerID != 777 || got.RunnerName != "runner-9" {
		t.Fatalf("MarkJobDispatched clobbered real binding: %+v", got)
	}

	// Completed jobs are not resurrected by a stale in_progress event.
	j3 := &Job{ID: 10, Repo: "a/b", Action: "queued", Labels: "x",
		DedupeKey: "10", Status: "pending"}
	if _, err := s.InsertJobIfNew(ctx, j3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkJobInProgress(ctx, 10, 1, "r"); err != nil {
		t.Fatal(err)
	}
	completed, err := s.MarkJobCompleted(ctx, 10, "success")
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("MarkJobCompleted completed=false, want true")
	}
	if _, err := s.MarkJobInProgress(ctx, 10, 999, "ghost"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, 10)
	if got.Status != "completed" || got.RunnerName != "r" {
		t.Fatalf("completed row was resurrected: %+v", got)
	}
}

func TestMarkJobCompleted_MissingJobNoOp(t *testing.T) {
	s := newStore(t)
	completed, err := s.MarkJobCompleted(context.Background(), 404, "success")
	if err != nil {
		t.Fatal(err)
	}
	if completed {
		t.Fatal("MarkJobCompleted completed=true for missing job, want false")
	}
}

func TestPendingJobs_ReplaysStaleDispatched(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// fresh-pending: included.
	jp := &Job{ID: 1, Repo: "a/b", Action: "queued", Labels: "x",
		DedupeKey: "1", Status: "pending"}
	if _, err := s.InsertJobIfNew(ctx, jp); err != nil {
		t.Fatal(err)
	}
	// fresh-dispatched (just now): NOT included — webhook may still arrive.
	jd := &Job{ID: 2, Repo: "a/b", Action: "queued", Labels: "x",
		DedupeKey: "2", Status: "pending"}
	if _, err := s.InsertJobIfNew(ctx, jd); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJobDispatched(ctx, 2); err != nil {
		t.Fatal(err)
	}
	// stale-dispatched (>5min ago): included — runner never claimed.
	js := &Job{ID: 3, Repo: "a/b", Action: "queued", Labels: "x",
		DedupeKey: "3", Status: "pending"}
	if _, err := s.InsertJobIfNew(ctx, js); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJobDispatched(ctx, 3); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-1 * time.Hour)
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id=3`, stale); err != nil {
		t.Fatal(err)
	}
	// in_progress: NOT included.
	ji := &Job{ID: 4, Repo: "a/b", Action: "queued", Labels: "x",
		DedupeKey: "4", Status: "pending"}
	if _, err := s.InsertJobIfNew(ctx, ji); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkJobInProgress(ctx, 4, 1, "r"); err != nil {
		t.Fatal(err)
	}

	out, err := s.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := make(map[int64]bool, len(out))
	for _, j := range out {
		gotIDs[j.ID] = true
	}
	if !gotIDs[1] {
		t.Errorf("missing fresh-pending id=1")
	}
	if gotIDs[2] {
		t.Errorf("included fresh-dispatched id=2 (should wait for replay window)")
	}
	if !gotIDs[3] {
		t.Errorf("missing stale-dispatched id=3")
	}
	if gotIDs[4] {
		t.Errorf("included in_progress id=4")
	}
}

func TestPendingJobs_Order(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// Insert in any order, then explicitly stamp distinct received_at so
	// the test does not depend on wall-clock sleeps or sqlite's
	// second-precision CURRENT_TIMESTAMP.
	for _, id := range []int64{3, 1, 2} {
		j := &Job{ID: id, Repo: "a/b", Action: "queued", Labels: "x",
			DedupeKey: itoa(id), Status: "pending"}
		if _, err := s.InsertJobIfNew(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	// received_at order: id=3 first, then id=1, then id=2.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for offset, id := range []int64{3, 1, 2} {
		when := base.Add(time.Duration(offset) * time.Second)
		if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET received_at=? WHERE id=?`, when, id); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.PendingJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].ID != 3 || out[1].ID != 1 || out[2].ID != 2 {
		t.Fatalf("order = [%d %d %d]", out[0].ID, out[1].ID, out[2].ID)
	}
}

func TestActiveRunnerCount_StatusFilter(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now()
	statuses := []string{"starting", "idle", "busy", "finished"}
	for i, st := range statuses {
		r := &Runner{ContainerName: "c" + itoa(int64(i)), Repo: "a/b",
			RunnerName: "n" + itoa(int64(i)), Labels: "x", Status: st, StartedAt: now}
		if err := s.InsertRunner(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ActiveRunnerCount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3 (starting,idle,busy)", n)
	}
	active, _ := s.ListActiveRunners(ctx)
	if len(active) != 3 {
		t.Fatalf("ListActiveRunners len = %d, want 3", len(active))
	}
}

func TestUpdateRunnerStatus_FinishedSetsFinishedAt(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	r := &Runner{ContainerName: "c1", Repo: "a/b", RunnerName: "n1",
		Labels: "x", Status: "starting", StartedAt: time.Now()}
	if err := s.InsertRunner(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunnerStatus(ctx, "c1", "finished"); err != nil {
		t.Fatal(err)
	}
	var st string
	var fin *time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT status, finished_at FROM runners WHERE container_name='c1'`).Scan(&st, &fin); err != nil {
		t.Fatal(err)
	}
	if st != "finished" || fin == nil {
		t.Fatalf("status=%s finished_at=%v", st, fin)
	}
}

func TestUpdateRunnerStatusByName_MatchesRunnerNameNotContainer(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	r := &Runner{ContainerName: "container-xyz", Repo: "a/b", RunnerName: "runner-abc",
		Labels: "x", Status: "starting", StartedAt: time.Now()}
	if err := s.InsertRunner(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunnerStatusByName(ctx, "runner-abc", "busy"); err != nil {
		t.Fatal(err)
	}
	var st string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM runners WHERE container_name='container-xyz'`).Scan(&st); err != nil {
		t.Fatal(err)
	}
	if st != "busy" {
		t.Fatalf("status = %s, want busy", st)
	}
}

// CancelJobIfPending must NOT overwrite a row that's already
// terminal — that's the entire point: a concurrent webhook may have
// written the real conclusion (success/failure/cancelled), and
// dispatch's defensive cancel paths must yield to that truth.
func TestCancelJobIfPending_SkipsTerminalRows(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	rows := []*Job{
		{ID: 1, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "1", Status: "pending"},
		{ID: 2, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "2", Status: "dispatched"},
		{ID: 3, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "3", Status: "in_progress"},
		{ID: 4, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "4", Status: "completed", Conclusion: "success"},
	}
	for _, j := range rows {
		if _, err := s.InsertJobIfNew(ctx, j); err != nil {
			t.Fatalf("seed %d: %v", j.ID, err)
		}
	}

	for _, tc := range []struct {
		id            int64
		wantCancelled bool
		wantStatus    string
		wantConcl     string
	}{
		{1, true, "completed", "cancelled"},
		{2, true, "completed", "cancelled"},
		{3, false, "in_progress", ""},
		{4, false, "completed", "success"}, // must not overwrite real conclusion
	} {
		got, err := s.CancelJobIfPending(ctx, tc.id)
		if err != nil {
			t.Fatalf("id=%d: %v", tc.id, err)
		}
		if got != tc.wantCancelled {
			t.Errorf("id=%d cancelled=%v, want %v", tc.id, got, tc.wantCancelled)
		}
		j, err := s.GetJob(ctx, tc.id)
		if err != nil || j == nil {
			t.Fatalf("id=%d get: %v / %v", tc.id, err, j)
		}
		if j.Status != tc.wantStatus || j.Conclusion != tc.wantConcl {
			t.Errorf("id=%d after cancel: status=%q conclusion=%q, want status=%q conclusion=%q",
				tc.id, j.Status, j.Conclusion, tc.wantStatus, tc.wantConcl)
		}
	}
}

// Repo-scoped cancel: only pending/dispatched rows for that repo
// transition; rows for other repos and rows already in terminal states
// must be left alone (otherwise we'd retroactively rewrite history of
// jobs that already ran).
func TestCancelPendingJobsForRepo_OnlyPendingAndDispatched(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	rows := []*Job{
		{ID: 1, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "1", Status: "pending"},
		{ID: 2, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "2", Status: "dispatched"},
		{ID: 3, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "3", Status: "in_progress"},
		{ID: 4, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "4", Status: "completed"},
		{ID: 5, Repo: "other/repo", Action: "queued", Labels: "x", DedupeKey: "5", Status: "pending"},
	}
	for _, j := range rows {
		if _, err := s.InsertJobIfNew(ctx, j); err != nil {
			t.Fatalf("seed %d: %v", j.ID, err)
		}
	}

	n, err := s.CancelPendingJobsForRepo(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("affected rows = %d, want 2 (the pending + dispatched on a/b)", n)
	}

	type row struct {
		status, conclusion string
	}
	got := map[int64]row{}
	for _, id := range []int64{1, 2, 3, 4, 5} {
		j, err := s.GetJob(ctx, id)
		if err != nil || j == nil {
			t.Fatalf("get %d: %v / %v", id, err, j)
		}
		got[id] = row{j.Status, j.Conclusion}
	}
	if got[1] != (row{"completed", "cancelled"}) || got[2] != (row{"completed", "cancelled"}) {
		t.Fatalf("pending+dispatched rows wrong: %+v", got)
	}
	if got[3].status != "in_progress" || got[4].status != "completed" {
		t.Fatalf("non-pending rows on a/b mutated: %+v", got)
	}
	if got[5] != (row{"pending", ""}) {
		t.Fatalf("other-repo row mutated: %+v", got[5])
	}
}

func TestListJobs_FilterOrderAndLimit(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	seed := []*Job{
		{ID: 1, Repo: "a/repo", JobName: "lint", RunID: 1001, RunAttempt: 1, WorkflowName: "CI", Action: "queued", Labels: "x", DedupeKey: "1", PayloadJSON: `{"id":1}`, Status: "pending"},
		{ID: 2, Repo: "a/repo", JobName: "test", RunID: 1001, RunAttempt: 1, WorkflowName: "CI", Action: "queued", Labels: "x", DedupeKey: "2", PayloadJSON: `{"id":2}`, Status: "completed"},
		{ID: 3, Repo: "b/repo", JobName: "build", RunID: 1002, RunAttempt: 3, WorkflowName: "Release", Action: "queued", Labels: "x", DedupeKey: "3", PayloadJSON: `{"id":3}`, Status: "pending"},
		{ID: 4, Repo: "b/repo", JobName: "deploy", RunID: 1003, RunAttempt: 2, WorkflowName: "Deploy", Action: "queued", Labels: "x", DedupeKey: "4", PayloadJSON: `{"id":4}`, Status: "dispatched"},
	}
	for _, j := range seed {
		if _, err := s.InsertJobIfNew(ctx, j); err != nil {
			t.Fatalf("seed %d: %v", j.ID, err)
		}
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// id=4 newest, then 3, then 2, then 1.
	for idx, id := range []int64{1, 2, 3, 4} {
		when := base.Add(time.Duration(idx) * time.Second)
		if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id=?`, when, id); err != nil {
			t.Fatalf("set updated_at id=%d: %v", id, err)
		}
	}

	all, err := s.ListJobs(ctx, JobListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("len(all)=%d, want 4", len(all))
	}
	if all[0].ID != 4 || all[1].ID != 3 || all[2].ID != 2 || all[3].ID != 1 {
		t.Fatalf("unexpected order: [%d %d %d %d]", all[0].ID, all[1].ID, all[2].ID, all[3].ID)
	}
	if all[0].JobName != "deploy" || all[0].WorkflowName != "Deploy" || all[0].RunAttempt != 2 || all[0].PayloadJSON == "" {
		t.Fatalf("metadata not loaded from row: %+v", all[0])
	}

	pending, err := s.ListJobs(ctx, JobListFilter{Statuses: []string{"pending"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != 3 || pending[1].ID != 1 {
		t.Fatalf("pending mismatch: %+v", pending)
	}

	byRepo, err := s.ListJobs(ctx, JobListFilter{Repo: "a/repo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byRepo) != 2 || byRepo[0].ID != 2 || byRepo[1].ID != 1 {
		t.Fatalf("repo mismatch: %+v", byRepo)
	}

	limited, err := s.ListJobs(ctx, JobListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 || limited[0].ID != 4 || limited[1].ID != 3 {
		t.Fatalf("limit mismatch: %+v", limited)
	}
}

func TestSummary_CountsJobsAndRunners(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	for _, j := range []*Job{
		{ID: 1, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "1", Status: "pending"},
		{ID: 2, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "2", Status: "pending"},
		{ID: 3, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "3", Status: "completed"},
	} {
		if _, err := s.InsertJobIfNew(ctx, j); err != nil {
			t.Fatalf("insert job %d: %v", j.ID, err)
		}
	}
	for _, r := range []*Runner{
		{ContainerName: "c1", Repo: "a/b", RunnerName: "r1", Labels: "x", Status: "starting", StartedAt: time.Now()},
		{ContainerName: "c2", Repo: "a/b", RunnerName: "r2", Labels: "x", Status: "busy", StartedAt: time.Now()},
		{ContainerName: "c3", Repo: "a/b", RunnerName: "r3", Labels: "x", Status: "finished", StartedAt: time.Now()},
	} {
		if err := s.InsertRunner(ctx, r); err != nil {
			t.Fatalf("insert runner %s: %v", r.ContainerName, err)
		}
	}

	got, err := s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.JobsByStatus["pending"] != 2 || got.JobsByStatus["completed"] != 1 {
		t.Fatalf("jobs by status = %+v", got.JobsByStatus)
	}
	if got.RunnersByStatus["starting"] != 1 || got.RunnersByStatus["busy"] != 1 || got.RunnersByStatus["finished"] != 1 {
		t.Fatalf("runners by status = %+v", got.RunnersByStatus)
	}
}

func TestRetryJobIfCompleted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	j := &Job{ID: 55, Repo: "a/b", Action: "queued", Labels: "x", DedupeKey: "55", Status: "completed", Conclusion: "failure", RunnerID: 42, RunnerName: "runner-55"}
	if _, err := s.InsertJobIfNew(ctx, j); err != nil {
		t.Fatal(err)
	}

	retried, err := s.RetryJobIfCompleted(ctx, 55)
	if err != nil {
		t.Fatal(err)
	}
	if !retried {
		t.Fatal("retried=false, want true")
	}

	got, err := s.GetJob(ctx, 55)
	if err != nil || got == nil {
		t.Fatalf("GetJob: %v / %+v", err, got)
	}
	if got.Status != "pending" || got.Conclusion != "" || got.RunnerID != 0 || got.RunnerName != "" {
		t.Fatalf("retry transition mismatch: %+v", got)
	}

	retried, err = s.RetryJobIfCompleted(ctx, 55)
	if err != nil {
		t.Fatal(err)
	}
	if retried {
		t.Fatal("retried=true on non-completed row, want false")
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
