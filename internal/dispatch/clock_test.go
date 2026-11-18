package dispatch

import (
	"testing"
	"time"
)

func TestDomainClockReadsOffsetOnce(t *testing.T) {
	for _, tc := range []struct {
		version, raw string
		offset       time.Duration
	}{
		{"test", "-1h30m", -90 * time.Minute},
		{"dev", "+30s", 30 * time.Second},
		{"dev", "", 0},
		{"v1.2.3", "invalid", 0},
	} {
		t.Run(tc.version+"/"+tc.raw, func(t *testing.T) {
			t.Setenv(clockOffsetEnv, tc.raw)
			now, err := DomainClock(tc.version)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv(clockOffsetEnv, "6h")
			before := time.Now().Add(tc.offset)
			got := now()
			if got.Before(before) || got.After(time.Now().Add(tc.offset)) {
				t.Fatalf("clock lost its startup offset of %s", tc.offset)
			}
		})
	}
}
