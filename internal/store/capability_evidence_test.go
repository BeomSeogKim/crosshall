package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCapabilityCoverageUsesExactEvidenceBeforeManifestFallback(t *testing.T) {
	for _, tc := range []struct {
		name, field, value, exact string
		stopped, lifted           bool
		covered                   bool
	}{
		{name: "exact tested", exact: "tested", covered: true},
		{name: "other version tested", covered: true},
		{name: "exact candidate", exact: "candidate"},
		{name: "exact unavailable", exact: "unavailable"},
		{name: "stopped", stopped: true},
		{name: "stopped even with exact tested evidence", exact: "tested", stopped: true},
		{name: "lifted in same millisecond", stopped: true, lifted: true, covered: true},
		{name: "different revision", field: "manifest_rev", value: "claude-3"},
		{name: "different platform", field: "platform", value: "linux-arm64"},
		{name: "different mode", field: "mode", value: "print"},
		{name: "different adapter", field: "adapter", value: "codex"},
		{name: "unverified version", field: "vendor_version", value: "unverified"},
		{name: "unverified mode", field: "mode", value: "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			db, err := Open(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			profile := map[string]string{"adapter": "claude", "vendor_version": "2", "platform": "darwin-arm64", "mode": "interactive", "manifest_rev": "claude-2"}
			if tc.field != "" {
				profile[tc.field] = tc.value
			}
			raw, err := json.Marshal(profile)
			if err != nil {
				t.Fatal(err)
			}
			for _, cap := range []string{"publish", "turn_end", "observe"} {
				err := db.Update(ctx, func(tx Tx) error {
					if _, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
						(vendor, vendor_version, platform, mode, manifest_rev, cap, level, checked_at_ms, result_ref)
						VALUES ('claude', '1', 'darwin-arm64', 'interactive', 'claude-2', ?, 'tested', 4, 'first')`, cap); err != nil {
						return err
					}
					if tc.exact != "" {
						if _, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
							(vendor, vendor_version, platform, mode, manifest_rev, cap, level, checked_at_ms, result_ref)
							VALUES ('claude', '2', 'darwin-arm64', 'interactive', 'claude-2', ?, ?, 5, 'exact')`, cap, tc.exact); err != nil {
							return err
						}
					}
					if tc.stopped {
						var lift any
						if tc.lifted {
							lift = 10
						}
						if _, err := tx.ExecContext(ctx, `INSERT INTO capability_build
							(vendor, vendor_version, platform, mode, manifest_rev, cap, stopped_at_ms, stop_reason, stop_ref, lifted_at_ms)
							VALUES ('claude', '2', 'darwin-arm64', 'interactive', 'claude-2', ?, 10, 'unknown', 41, ?)`, cap, lift); err != nil {
							return err
						}
					}
					build, err := readCapabilityBuild(ctx, tx, profile["adapter"], string(raw), cap)
					if err == nil && build.Covered != tc.covered {
						t.Errorf("%s covered %t, want %t", cap, build.Covered, tc.covered)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCapabilityBuildMigrationPreservesExactEvidence(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	old, err := openWithMigrations(ctx, root, migrations[:15])
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.db.ExecContext(ctx, `INSERT INTO capability_evidence
		(vendor, vendor_version, platform, mode, manifest_rev, cap, level, checked_at_ms, result_ref)
		VALUES ('claude', '2', 'darwin-arm64', 'interactive', 'claude-2', 'publish', 'candidate', 8, 'owner')`)
	if err != nil {
		old.Close()
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var level string
	var builds int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT level, (SELECT COUNT(*) FROM capability_build) FROM capability_evidence
		WHERE vendor = 'claude' AND vendor_version = '2' AND checked_at_ms = 8 AND source = 'owner'`).Scan(&level, &builds); err != nil || level != "candidate" || builds != 0 {
		t.Fatalf("upgraded evidence %q and build count %d (%v)", level, builds, err)
	}
}

func TestOwnerViewsReportManifestBuildStates(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedAdapterLedger(t, db)
	err = db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms)
			VALUES ('claude', 1, 0, 1, 2) ON CONFLICT (adapter) DO UPDATE SET enabled = 1, epoch = 1;
			INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms) VALUES
			(2, 'first', '{"adapter":"claude","vendor_version":"2","platform":"darwin-arm64","mode":"interactive","manifest_rev":"claude-2"}', 1, 2);
			INSERT INTO publish_route (binding_id, principal_id, generation, adapter, session_ref, launch_ref, launch_verified,
				policy_epoch, mode, socket_path, watermark_message_id, state, registered_at_ms, updated_at_ms) VALUES
			(2, 2, 1, 'claude', 'session', 'first', 1, 1, 'default', '/tmp/cc-socks/fixture.sock', 0, 'active', 2, 2);
			INSERT INTO publish_route_proof (binding_id, generation, process_pid, process_start_ms, activated_at_ms)
			VALUES (2, 1, 22, 10, 2);
			INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, checked_at_ms, result_ref)
			VALUES ('claude', '1', 'darwin-arm64', 'interactive', 'claude-2', 'publish', 'tested', 3, 'tested'),
			       ('claude', '1', 'darwin-arm64', 'interactive', 'claude-2', 'turn_end', 'tested', 3, 'tested');
			INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, confirmed)
			VALUES ('claude', '2', 'darwin-arm64', 'interactive', 'claude-2', 'publish', 1);`)
		if err != nil {
			return err
		}
		now := time.UnixMilli(1000)
		check := func(routeState, reason, buildState string) {
			t.Helper()
			route, found, err := ReadPublishState(ctx, tx, 2, now)
			if err != nil || !found || route.State != routeState || route.Reason != reason {
				t.Fatalf("member state %+v, want %q (%q), error %v", route, routeState, reason, err)
			}
			var doctor DoctorAdapterState
			doctor.Policy.Enabled = true
			if err := readDoctorBuilds(ctx, tx, "claude", &doctor); err != nil {
				t.Fatal(err)
			}
			if len(doctor.Publish.Builds) != 1 || doctor.Publish.Builds[0].State != buildState ||
				!doctor.TurnEndNotice.Available || doctor.TurnEndNotice.TrustedFrom != "1" {
				t.Fatalf("doctor publish builds %+v and turn-end %+v, want %s and borrowed turn-end evidence",
					doctor.Publish.Builds, doctor.TurnEndNotice, buildState)
			}
			if buildState == "stopped" && !strings.Contains(route.Lift, "crosshall owner publish resume 2 or tested evidence") {
				t.Fatalf("unexpected resume guidance %q", route.Lift)
			}
		}
		check("available", "version 2 trusted from claude-2 evidence, 1 of 3 first notices observed", "trusted")
		if _, err := tx.ExecContext(ctx, `UPDATE capability_build SET stopped_at_ms = 11, stop_reason = 'absorbed', stop_ref = 41`); err != nil {
			return err
		}
		check("suspended", "version 2 stopped after notice 41 (absorbed)", "stopped")
		if _, err := tx.ExecContext(ctx, `UPDATE capability_build SET lifted_at_ms = 12, confirmed = 3`); err != nil {
			return err
		}
		check("available", "version 2 seen", "seen")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDoctorCountsUnobservedNoticesByTrustedBuild(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedAdapterLedger(t, db)
	err = db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
			(vendor, vendor_version, platform, mode, manifest_rev, cap, level, checked_at_ms, result_ref)
			VALUES ('claude', '1', 'darwin-arm64', 'interactive', 'claude-2', 'publish', 'tested', 3, 'fixture')`)
		if err != nil {
			return err
		}
		for i, attempt := range []struct {
			version, state        string
			observedAt, retiredAt any
		}{
			{version: "2", state: "submitted"},
			{version: "2", state: "uncertain"},
			{version: "2", state: "submitting"},
			{version: "2", state: "submitted", observedAt: 6},
			{version: "2", state: "uncertain", retiredAt: 6},
			{version: "2", state: "not_submitted"},
			{version: "3", state: "submitted"},
			{version: "4", state: "reserved"},
			{version: "1", state: "submitted"},
		} {
			id := i + 4
			_, err := tx.ExecContext(ctx, `INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (?1, 'session', 'claude', 1);
				INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (?1, ?1, 1, 'writer-' || ?1, 1);
				INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms) VALUES
					(?1, 'fixture', json_object('adapter', 'claude', 'vendor_version', ?2, 'platform', 'darwin-arm64',
						'mode', 'interactive', 'manifest_rev', 'claude-2'), 1, 1);
				INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref,
					launch_ref, policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms,
					observed_at_ms, retired_at_ms)
				VALUES (?1, ?1, 1, 1, 'claude', 'session', 'fixture', 1, ?1, 2, ?3, 3,
					CASE WHEN ?3 <> 'reserved' THEN 4 END, ?4, ?5)`,
				id, attempt.version, attempt.state, attempt.observedAt, attempt.retiredAt)
			if err != nil {
				return err
			}
		}
		var doctor DoctorAdapterState
		if err := readDoctorBuilds(ctx, tx, "claude", &doctor); err != nil {
			return err
		}
		builds := doctor.Publish.Builds
		if len(builds) != 4 || builds[0].Version != "1" || builds[0].State != "tested" || builds[0].HeldUnobserved != nil {
			t.Fatalf("publish builds: %+v", builds)
		}
		for i, want := range []int{3, 1, 0} {
			build := builds[i+1]
			if build.State != "trusted" || build.HeldUnobserved == nil || *build.HeldUnobserved != want {
				t.Errorf("build %s: %+v, want %d held unobserved", build.Version, build, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
