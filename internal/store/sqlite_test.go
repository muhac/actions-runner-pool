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
	if err := s.MarkJobInProgress(ctx, 7, 99, "runner-7"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetJob(ctx, 7)
	if got.Status != "in_progress" || got.RunnerID != 99 || got.RunnerName != "runner-7" {
		t.Fatalf("after in_progress: %+v", got)
	}
	if err := s.MarkJobCompleted(ctx, 7, "success"); err != nil {
		t.Fatal(err)
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
	if err := s.MarkJobInProgress(ctx, 8, 555, "runner-8"); err != nil {
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
	if err := s.MarkJobInProgress(ctx, 9, 777, "runner-9"); err != nil {
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
	if err := s.MarkJobInProgress(ctx, 10, 1, "r"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJobCompleted(ctx, 10, "success"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkJobInProgress(ctx, 10, 999, "ghost"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetJob(ctx, 10)
	if got.Status != "completed" || got.RunnerName != "r" {
		t.Fatalf("completed row was resurrected: %+v", got)
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
	if err := s.MarkJobInProgress(ctx, 4, 1, "r"); err != nil {
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
