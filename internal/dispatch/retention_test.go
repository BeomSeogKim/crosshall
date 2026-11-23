package dispatch

import (
	"testing"
	"time"
)

// The retention pass runs once per UTC day, at the first tick of a later
// day, and a clock set back runs no earlier pass (#256).
func TestRetentionRunsOncePerUTCDay(t *testing.T) {
	day := time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	for _, c := range []struct {
		name      string
		last, now time.Time
		want      bool
	}{
		{"first pass", time.Time{}, day, true},
		{"same UTC day", day, day.Add(59 * time.Minute), false},
		{"next UTC day", day, day.Add(time.Hour), true},
		{"clock set back", day, day.Add(-48 * time.Hour), false},
		{"local midnight is not UTC", day, day.In(kst).Add(30 * time.Minute), false},
		{"the next UTC day in another zone", day.In(kst), day.Add(90 * time.Minute).In(kst), true},
	} {
		if got := retentionDue(c.last, c.now); got != c.want {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
}
