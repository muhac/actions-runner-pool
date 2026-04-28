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
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET updated_at=? WHERE id=?`, t, jobID)
	return err
}
