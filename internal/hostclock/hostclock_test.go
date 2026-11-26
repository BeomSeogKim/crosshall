package hostclock

import (
	"testing"
	"time"
)

// The boot stays the same within a process, and the clock never goes back.
func TestTheBootIsStableAndTheClockMonotonic(t *testing.T) {
	first, err := Boot()
	if err != nil || first == "" {
		t.Fatalf("boot %q, %v", first, err)
	}
	a, err := Mono()
	if err != nil || a <= 0 {
		t.Fatalf("mono %d, %v", a, err)
	}
	time.Sleep(2 * time.Millisecond)
	b, _ := Mono()
	second, _ := Boot()
	if second != first || b-a < int64(time.Millisecond) {
		t.Fatalf("boot %q then %q; mono %d then %d", first, second, a, b)
	}
}
