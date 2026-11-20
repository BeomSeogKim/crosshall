package proc

import (
	"errors"
	"testing"
)

// Fable #448: a Linux start read with two boot estimates (two processes,
// or one after a clock step) names the same process.
func TestLinuxStartIdentityIgnoresTheBootEstimate(t *testing.T) {
	const stat = "4242 (claude (code) x) S 1 4242 4242 0 -1 4194560 100 0 0 0 5 1 0 0 20 0 9 0 123456 1000 50"
	first, err := linuxStart(stat, 1_700_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	second, err := linuxStart(stat, 1_700_000_003_570)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity != second.Identity || first.Identity != 123457 {
		t.Fatalf("identities %d and %d, want both 123457", first.Identity, second.Identity)
	}
	if first.AtMS == second.AtMS || first.AtMS != 1_700_000_000_000+1_234_560-startErrorMS {
		t.Fatalf("wall-clock starts %d and %d should follow their estimates", first.AtMS, second.AtMS)
	}
	if _, err := linuxStart("4242 (x) Z 1 4242 4242 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 5", 1); !errors.Is(err, errProcessGone) {
		t.Fatalf("a zombie: %v", err)
	}
	for _, bad := range []string{"no paren", "1 (x) S 1", "1 (x) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 -5"} {
		if _, err := linuxStart(bad, 1); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
