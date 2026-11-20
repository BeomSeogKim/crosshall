package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// newPolicyFixture is the domain fixture, which applies every migration.
func newPolicyFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t)
}

func TestAdapterPolicyDefaultsAndChanges(t *testing.T) {
	f := newPolicyFixture(t)
	on, off := true, false
	set := func(key string, enabled, reminder *bool) store.AdapterPolicy {
		t.Helper()
		got := f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "codex", Enabled: enabled, Reminder: reminder}}, key)
		wantOK(t, got)
		return decode[store.AdapterPolicy](t, got)
	}
	if got := set("default-off", &off, nil); got != (store.AdapterPolicy{Adapter: "codex"}) {
		t.Fatalf("implicit policy: %+v", got)
	}
	if n := f.count(`SELECT count(*) FROM adapter_policy`); n != 0 {
		t.Fatalf("an unchanged implicit policy created %d rows", n)
	}
	first := set("enable", &on, nil)
	if first != (store.AdapterPolicy{Adapter: "codex", Enabled: true, Epoch: 1, ChangedAtMS: f.now.UnixMilli()}) {
		t.Fatalf("enabled policy: %+v", first)
	}
	f.now = f.now.Add(time.Second)
	if got := set("no-change", &on, &off); got != first {
		t.Fatalf("unchanged policy: %+v", got)
	}
	second := set("disable", &off, &on)
	if second != (store.AdapterPolicy{Adapter: "codex", Reminder: true, Epoch: 2, ChangedAtMS: f.now.UnixMilli()}) {
		t.Fatalf("disabled policy: %+v", second)
	}
	if got := set("enable", &on, nil); got != first {
		t.Fatalf("replayed operation returned %+v, want %+v", got, first)
	}
	if got := set("inspect-disabled", &off, nil); got != second {
		t.Fatalf("replay changed policy: %+v", got)
	}
	wantCode(t, f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "codex", Reminder: &off}}, "enable"),
		core.ExitRefused, core.CodeKeyConflict)
	if n := f.count(`SELECT count(*) FROM event WHERE kind = 'adapter_policy_set'`); n != 2 {
		t.Fatalf("got %d policy changes, want 2", n)
	}
}

func TestAdapterPolicyRefusesNonOwnerAndInvalidRequests(t *testing.T) {
	f := newPolicyFixture(t)
	on := true
	set := AdapterSet{wire.AdapterPolicyArgs{Adapter: "claude", Enabled: &on}}
	wantCode(t, f.exec(core.Caller{Role: core.RoleMember}, Request{Command: set, OpKey: "non-owner"}),
		core.ExitRefused, core.CodeOwnerOnly)
	wantCode(t, f.owner(set, ""), core.ExitUsage, "")
	wantCode(t, f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "agy", Enabled: &on}}, "unknown-adapter"),
		core.ExitUsage, "")
	wantCode(t, f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "claude"}}, "no-options"), core.ExitUsage, "")
	if n := f.count(`SELECT count(*) FROM adapter_policy`); n != 0 {
		t.Fatalf("refusals left %d policy rows", n)
	}
}

func TestAdapterPolicyRebuildsMissingCapabilities(t *testing.T) {
	f, _ := adapterChannel(t)
	on, off := true, false
	set := func(enabled bool, key string) {
		t.Helper()
		wantOK(t, f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "codex", Enabled: &enabled, Reminder: &on}}, key))
	}
	set(on, "enable")
	session := f.newSession("codex")
	enroll := start(SessionLaunch, "startup", ms(f.now))
	enroll.Profile = wire.HookProfile{Adapter: "codex", VendorVersion: "1.0", Platform: "test-platform", Mode: "exec", ManifestRev: "r2"}
	if got := f.hook(session, enroll); got.Enrollment != "joined" {
		t.Fatalf("hook enrollment: %+v", got)
	}
	principal := *f.caller(session).PrincipalID
	// Simulate an existing installation whose launch predates capability projection.
	if _, err := f.db.Exec(`DELETE FROM capability WHERE principal_id = ?`, principal); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO capability_evidence(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
		VALUES ('codex', '1.0', 'test-platform', 'exec', 'r2', 'enroll', 'tested', 'fixture', 10),
		       ('codex', '1.0', 'test-platform', 'exec', 'r2', 'observe', 'unavailable', 'fixture', 11),
		       ('codex', '1.0', 'test-platform', 'exec', 'r1', 'boundary', 'unavailable', 'older manifest', 12),
		       ('codex', '1.0', 'test-platform', 'exec', 'r2', 'reminder', 'tested', 'fixture', 13)`); err != nil {
		t.Fatal(err)
	}

	check := func(enabled bool) {
		t.Helper()
		for _, want := range []struct {
			cap, level string
			available  bool
			checked    int64
		}{
			{"enroll", "tested", enabled, 10},
			{"observe", "unavailable", false, 11},
			{"boundary", "candidate", enabled, enroll.HookAtMS},
			{"wake", "unavailable", false, enroll.HookAtMS},
		} {
			var level, version, platform, mode string
			var available bool
			var checked int64
			err := f.db.QueryRow(`SELECT level, available, checked_at_ms, vendor_version, platform, mode
				FROM capability WHERE principal_id = ? AND cap = ?`, principal, want.cap).
				Scan(&level, &available, &checked, &version, &platform, &mode)
			if err != nil {
				t.Fatalf("%s projection: %v", want.cap, err)
			}
			if level != want.level || available != want.available || checked != want.checked ||
				version != "1.0" || platform != "test-platform" || mode != "exec" {
				t.Errorf("%s projection = (%s, %v, %d, %s, %s, %s), want (%s, %v, %d, 1.0, test-platform, exec)",
					want.cap, level, available, checked, version, platform, mode, want.level, want.available, want.checked)
			}
		}
	}

	set(off, "disable")
	check(off)
	set(on, "reenable")
	check(on)
}

func TestRefreshMemberAdapterCapabilitiesLeavesOtherMembersAlone(t *testing.T) {
	f, _ := adapterChannel(t)
	member := func(alias string) int64 {
		t.Helper()
		s := f.newSession("claude")
		wantOK(t, f.join(s, Join{Channel: "checkout", Alias: alias}, alias))
		return *f.caller(s).PrincipalID
	}
	principal, other := member("current"), member("other")

	ctx := context.Background()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, id := range []int64{principal, other} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms)
			VALUES (?, 'fixture', ?, 1, ?)`, id, profileJSON(testProfile), ms(f.now)); err != nil {
			t.Fatal(err)
		}
	}
	if err := refreshMemberAdapterCapabilities(ctx, tx, principal, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if n := f.count(`SELECT count(*) FROM capability WHERE principal_id = ? AND level = 'candidate' AND available = 1`, principal); n != 3 {
		t.Fatalf("member refresh projected %d available candidates, want 3", n)
	}
	if n := f.count(`SELECT count(*) FROM capability WHERE principal_id = ?`, other); n != 0 {
		t.Fatalf("member refresh wrote %d rows for another session", n)
	}
}

func TestAdapterPolicyRefreshesCurrentLiveLaunchCapabilities(t *testing.T) {
	f := newPolicyFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "dev"}, "channel"))
	exec := func(statement string, args ...any) {
		t.Helper()
		if _, err := f.db.Exec(statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	member := func(vendor, alias string) int64 {
		t.Helper()
		s := f.newSession(vendor)
		wantOK(t, f.join(s, Join{Channel: "dev", Alias: alias}, alias))
		id := *f.caller(s).PrincipalID
		for _, name := range []string{"enroll", "observe", "boundary", "wake"} {
			exec(`INSERT INTO capability VALUES (?, ?, 'candidate', 0, 'old', 'old', 'old', 5)`, id, name)
		}
		return id
	}
	launch := func(principal int64, ref, vendor, version, mode string, start int64, end sql.NullInt64) {
		t.Helper()
		profile, err := json.Marshal(wire.HookProfile{Adapter: vendor, VendorVersion: version,
			Platform: "test-platform", Mode: mode, ManifestRev: "r1"})
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO adapter_launch(principal_id, launch_ref, profile, verified, started_hook_at_ms, ended_hook_at_ms)
			VALUES (?, ?, ?, 1, ?, ?)`, principal, ref, string(profile), start, end)
	}
	active := member("codex", "active")
	ended := member("codex", "ended")
	survivor := member("codex", "survivor")
	other := member("claude", "other")
	rekeyed := member("claude", "rekeyed")
	launch(active, "z-previous", "codex", "old", "old", 2, sql.NullInt64{})
	launch(active, "a-current", "codex", "1.0", "exec", 2, sql.NullInt64{})
	launch(ended, "previous", "codex", "1.0", "exec", 1, sql.NullInt64{Int64: 2, Valid: true})
	launch(ended, "current", "codex", "1.0", "exec", 2, sql.NullInt64{Int64: 3, Valid: true})
	// A resumed copy ended while the first launch still runs: the first is current.
	launch(survivor, "first", "codex", "1.0", "exec", 1, sql.NullInt64{})
	launch(survivor, "resumed", "codex", "old", "old", 2, sql.NullInt64{Int64: 3, Valid: true})
	launch(other, "current", "claude", "1.0", "exec", 1, sql.NullInt64{})
	launch(rekeyed, "before-restore", "claude", "1.0", "exec", 1, sql.NullInt64{})
	exec(`UPDATE principal SET vendor = 'codex' WHERE id = ?`, rekeyed)
	exec(`INSERT INTO capability_evidence(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
		VALUES ('codex', '1.0', 'test-platform', 'exec', 'r1', 'enroll', 'tested', 'fixture', 7),
		       ('codex', '1.0', 'test-platform', 'exec', 'r1', 'observe', 'unavailable', 'fixture', 8),
		       ('codex', '1.0', 'test-platform', 'tui', 'r1', 'boundary', 'unavailable', 'fixture', 9)`)
	exec(`UPDATE capability SET available = 1 WHERE principal_id = ? AND cap = 'wake'`, active)
	exec(`UPDATE capability SET available = 1 WHERE principal_id IN (?, ?, ?)`, ended, other, rekeyed)

	on, off := true, false
	wantOK(t, f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "codex", Enabled: &on, Reminder: &on}}, "enable"))

	check := func(id int64, cap, level, version, mode string, available bool, checked int64) {
		t.Helper()
		var gotLevel, gotVersion, gotMode string
		var gotAvailable bool
		var gotChecked int64
		if err := f.db.QueryRow(`SELECT level, vendor_version, mode, available, checked_at_ms
			FROM capability WHERE principal_id = ? AND cap = ?`, id, cap).
			Scan(&gotLevel, &gotVersion, &gotMode, &gotAvailable, &gotChecked); err != nil {
			t.Fatal(err)
		}
		if gotLevel != level || gotVersion != version || gotMode != mode || gotAvailable != available || gotChecked != checked {
			t.Fatalf("%d/%s: got {%s %s %s %v %d}, want {%s %s %s %v %d}",
				id, cap, gotLevel, gotVersion, gotMode, gotAvailable, gotChecked, level, version, mode, available, checked)
		}
	}
	check(active, "enroll", "tested", "1.0", "exec", true, 7)
	check(active, "observe", "unavailable", "1.0", "exec", false, 8)
	check(active, "boundary", "candidate", "1.0", "exec", true, 2)
	check(active, "wake", "unavailable", "1.0", "exec", false, 2)
	check(ended, "enroll", "candidate", "old", "old", false, 5)
	check(survivor, "enroll", "tested", "1.0", "exec", true, 7)
	check(other, "enroll", "candidate", "old", "old", true, 5)
	check(rekeyed, "enroll", "candidate", "old", "old", false, 5)
	wantOK(t, f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "codex", Enabled: &off}}, "disable"))
	check(active, "enroll", "tested", "1.0", "exec", false, 7)
	check(active, "boundary", "candidate", "1.0", "exec", false, 2)
}
