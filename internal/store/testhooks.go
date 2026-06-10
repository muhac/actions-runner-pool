package store

import (
	"context"
	"time"
)

// SetJobUpdatedAt is a test-only helper that backdates the updated_at
// column. Real code never sets updated_at directly — the regular
// MarkJob* writers do that — but tests need to simulate the passage of
// time without actually waiting (e.g., for the dispatched-replay age
// to elapse).
//
// Not part of the Store interface; callers must hold a *SQLite. Lives
// in a non-_test.go file so tests in other packages (scheduler) can
// call it.
func (s *SQLite) SetJobUpdatedAt(ctx context.Context, jobID int64, t time.Time) error {
	// Written in the exact text format CURRENT_TIMESTAMP produces (UTC,
	// second precision). Binding the time.Time directly would store the
	// driver's local-zone representation, which breaks PendingJobs'
	// lexicographic `updated_at < datetime('now', ...)` comparison in
	// any UTC+ timezone — the backdated row would sort as newer than
	// the cutoff and never be replayed.
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id=?`,
		t.UTC().Format("2006-01-02 15:04:05"), jobID)
	return err
}
