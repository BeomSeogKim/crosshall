package dispatch

import "testing"

func TestLaunchSweepRequiresMatchingPIDNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, platform, dispatcher, launch string
		want                               bool
	}{
		{"linux same", "linux", "boot-a:1", "boot-a:1", true},
		{"linux different namespace", "linux", "boot-a:1", "boot-a:2", false},
		{"linux different boot", "linux", "boot-a:1", "boot-b:1", false},
		{"linux legacy", "linux", "boot-a:1", "", false},
		{"linux unavailable", "linux", "", "", false},
		{"darwin", "darwin", "", "", true},
		{"darwin foreign launch", "darwin", "", "boot-a:1", false},
		{"unsupported", "other", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameLaunchPIDNamespace(tc.platform, tc.dispatcher, tc.launch); got != tc.want {
				t.Fatalf("sameLaunchPIDNamespace = %t, want %t", got, tc.want)
			}
		})
	}
}
