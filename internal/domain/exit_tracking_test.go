package domain

import (
	"testing"
	"time"
)

func TestExitTrackingModes(t *testing.T) {
	for _, tc := range []struct {
		name, adapter, launch string
		host, enabled         bool
		want                  ExitTracking
	}{
		{name: "Claude process launch with its end hook enabled", adapter: "claude", launch: "pid-100-stable", enabled: true, want: ExitByHooks},
		{name: "Claude process launch, adapter off", adapter: "claude", launch: "pid-100-stable", want: ExitByProcess},
		{name: "Codex session launch with its end hook enabled", adapter: "codex", launch: "session", host: true, enabled: true, want: ExitByProcess},
		{name: "Codex session launch without a host", adapter: "codex", launch: "session", enabled: true, want: ExitUntracked},
		{name: "Codex session launch, adapter off", adapter: "codex", launch: "session", host: true, want: ExitByProcess},
		{name: "host without a launch", host: true, want: ExitByProcess},
		{name: "Antigravity startup hook", adapter: "agy", launch: "session", host: true, enabled: true, want: ExitByProcess},
		{name: "Antigravity startup hook without a host", adapter: "agy", launch: "session", enabled: true, want: ExitUntracked},
		{name: "no launch or host", want: ExitUntracked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostFixture(t)
			f.svc.LaunchPIDNamespace = func() string { return "" }
			f.svc.HasSessionEnd = func(adapter, revision string) bool {
				return (adapter == "claude" || adapter == "codex") && revision == adapter+"-1"
			}
			member := f.principal(f.worker)
			if !tc.host {
				if _, err := f.db.Exec(`DELETE FROM member_host WHERE principal_id = ?`, member); err != nil {
					t.Fatal(err)
				}
			}
			if tc.launch != "" {
				if _, err := f.db.Exec(`INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms)
					VALUES (?, ?, json_object('adapter', ?, 'manifest_rev', ?), ?, ?)`,
					member, tc.launch, tc.adapter, tc.adapter+"-1", tc.launch != SessionLaunch, ms(f.now)); err != nil {
					t.Fatal(err)
				}
			}
			if tc.enabled {
				if _, err := f.db.Exec(`INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms)
					VALUES (?, 1, 1, 1, ?)`, tc.adapter, ms(f.now)); err != nil {
					t.Fatal(err)
				}
			}
			result := f.command(f.worker, Who{ExitTracking: true}, "", nil)
			wantOK(t, result)
			if got := decode[WhoResult](t, result).ExitTracking; got != tc.want {
				t.Fatalf("exit tracking = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOwnerWhoUsesHostEvidenceAfterCommandGoesStale(t *testing.T) {
	f := newHostFixture(t)
	f.now = f.now.Add(31 * time.Minute)
	worker := func() OwnerMember {
		t.Helper()
		result := f.owner(OwnerWho{}, "")
		wantOK(t, result)
		for _, channel := range decode[OwnerWhoResult](t, result).Channels {
			for _, member := range channel.Members {
				if member.Alias == "worker" {
					return member
				}
			}
		}
		t.Fatal("missing @worker")
		return OwnerMember{}
	}
	if m := worker(); !m.EvidenceStale || m.HostAlive || m.ExitTracking != ExitByProcess {
		t.Fatalf("without positive process evidence: %+v", m)
	}
	f.svc.ProcessAlive = func(pid, identity int64) bool { return pid == 100 && identity == 7 }
	if m := worker(); m.EvidenceStale || !m.HostAlive || m.BindingState != "live" {
		t.Fatalf("with positive process evidence: %+v", m)
	}
}
