package dispatch

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// retentionDue reports whether the daily retention pass starts at now: once
// per UTC day, on the first tick of a day after the last pass's day. A
// clock set back starts no pass until it passes that day again.
func retentionDue(last, now time.Time) bool {
	if last.IsZero() {
		return true
	}
	y, m, d := last.UTC().Date()
	return !now.UTC().Before(time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC))
}

// retention is the daily pass's state between dispatcher ticks.
type retention struct {
	last time.Time
	pass *store.RetentionPass
}

// retentionTick runs one chunk of the daily retention pass (#256), starting
// the pass when it is due. The dispatcher loop calls it once per tick after
// the scan, never before the first ready marker, so a hook waits for one
// chunk at most. A failed pass is abandoned until the next day.
func (s *server) retentionTick(ctx context.Context, now time.Time) {
	if s.retention.pass == nil {
		if !retentionDue(s.retention.last, now) {
			return
		}
		s.retention.last = now
		pass, err := s.db.StartRetention(ctx, now)
		if err != nil {
			s.logFailure(err)
			return
		}
		s.retention.pass = pass
	}
	done, err := s.db.RetentionChunk(ctx, s.retention.pass)
	if done || err != nil {
		s.retention.pass = nil
	}
	s.logFailure(err)
}
