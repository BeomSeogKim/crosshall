package dispatch

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

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
		{"linux different boot and namespace", "linux", bootA + ":1", bootB + ":2", false, false},
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

func TestHostIsGoneOnlyWithProofInItsNamespace(t *testing.T) {
	const bootA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const bootB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	for _, tc := range []struct {
		name, platform, dispatcher, host string
		processGone, want                bool
	}{
		{"darwin, process gone", "darwin", "", "", true, true},
		{"darwin, process running", "darwin", "", "", false, false},
		{"darwin, foreign namespace", "darwin", "", bootA + ":1", true, false},
		{"linux same namespace, gone", "linux", bootA + ":1", bootA + ":1", true, true},
		{"linux same namespace, running", "linux", bootA + ":1", bootA + ":1", false, false},
		{"linux foreign namespace", "linux", bootA + ":1", bootA + ":2", true, false},
		{"linux unknown host namespace", "linux", bootA + ":1", "", true, false},
		{"linux unknown dispatcher namespace", "linux", "", bootA + ":1", true, false},
		{"linux previous boot", "linux", bootA + ":1", bootB + ":1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := domain.HostedMember{PID: 100, StartIdentity: 7, PIDNamespace: tc.host}
			probed := false
			got := hostGone(tc.platform, tc.dispatcher, m, func(pid, identity int64) bool {
				probed = true
				if pid != 100 || identity != 7 {
					t.Errorf("probed %d %d", pid, identity)
				}
				return tc.processGone
			})
			if got != tc.want {
				t.Fatalf("gone %t, want %t", got, tc.want)
			}
			if probed && tc.host != tc.dispatcher {
				t.Error("a host outside the dispatcher's namespace was probed")
			}
		})
	}
}
