package dispatch

import (
	"context"
	"time"
)

// retentionDue reports whether the daily retention pass runs at now: once
// per UTC day, on the first tick of a day after the last pass's day. A
// clock set back runs no pass until it passes that day again.
func retentionDue(last, now time.Time) bool {
	if last.IsZero() {
		return true
	}
	y, m, d := last.UTC().Date()
	return !now.UTC().Before(time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC))
}

// runRetention deletes the rows no reader needs any more (#256).
func (s *server) runRetention(ctx context.Context, now time.Time) {
	if !retentionDue(s.lastRetention, now) {
		return
	}
	s.lastRetention = now
	_, err := s.db.PruneRetention(ctx, now)
	s.logFailure(err)
}
