package dispatch

import "testing"

func TestLaunchSweepComparesPIDNamespaceAndBoot(t *testing.T) {
	const bootA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const bootB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	for _, tc := range []struct {
		name, platform, dispatcher, launch string
		wantSame, wantPrevious             bool
	}{
		{"linux same", "linux", bootA + ":1", bootA + ":1", true, false},
		{"linux different namespace", "linux", bootA + ":1", bootA + ":2", false, false},
		{"linux previous boot", "linux", bootA + ":1", bootB + ":1", false, true},
		{"linux previous boot other namespace", "linux", bootA + ":1", bootB + ":2", false, true},
		{"linux malformed boot", "linux", bootA + ":1", "invalid:1", false, false},
		{"linux malformed inode", "linux", bootA + ":1", bootB + ":0", false, false},
		{"linux legacy", "linux", bootA + ":1", "", false, false},
		{"linux unavailable", "linux", "", "", false, false},
		{"darwin", "darwin", "", "", true, false},
		{"darwin foreign launch", "darwin", "", bootA + ":1", false, false},
		{"unsupported", "other", "", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			same, previous := compareLaunchPIDNamespaces(tc.platform, tc.dispatcher, tc.launch)
			if same != tc.wantSame || previous != tc.wantPrevious {
				t.Fatalf("same namespace %t and previous boot %t, want %t and %t", same, previous, tc.wantSame, tc.wantPrevious)
			}
		})
	}
}
