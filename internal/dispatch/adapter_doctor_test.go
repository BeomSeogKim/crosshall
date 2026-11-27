package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/setup"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type reminderDiagnostic struct {
	Available          *bool
	Reason, Provenance string
	ReleaseState       string `json:"release_state"`
	ExtraModelRequests int    `json:"extra_model_requests"`
	Evidence           []struct {
		Version, Platform, Mode, Level string
		ManifestRev                    string `json:"manifest_rev"`
	}
}

func TestDoctorReportsTurnEndClaimsAndLiveAvailability(t *testing.T) {
	cfg, _ := doctorFixture(t)
	ctx := t.Context()
	db, err := store.Open(ctx, cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	update := func(t *testing.T, stmt string, args ...any) {
		t.Helper()
		if err := db.Update(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, stmt, args...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	update(t, `
		INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms)
		VALUES ('claude', 1, 0, 1, 1), ('codex', 1, 0, 1, 1);
		INSERT INTO channel(id, name, created_at_ms) VALUES (1, 'dev', 1);
		INSERT INTO principal(id, kind, vendor, created_at_ms) VALUES (1, 'session', 'claude', 1), (2, 'session', 'codex', 1);
		INSERT INTO binding(principal_id, channel_id, alias, state, joined_at_ms)
		VALUES (1, 1, 'doctor-claude', 'live', 1), (2, 1, 'doctor-codex', 'live', 1);
		INSERT INTO adapter_launch(principal_id, launch_ref, profile, verified, started_hook_at_ms) VALUES
		  (1, 'old-claude', '{"adapter":"claude","vendor_version":"2.1.327","platform":"darwin-arm64","mode":"print","manifest_rev":"claude-1"}', 1, 1),
		  (1, 'current-claude', '{"adapter":"claude","vendor_version":"2.1.327","platform":"darwin-arm64","mode":"print","manifest_rev":"claude-1"}', 1, 2),
		  (2, 'isolated-codex', '{"adapter":"codex","vendor_version":"0.170.0","platform":"darwin-arm64","mode":"exec","manifest_rev":"codex-1"}', 0, 1),
		  (2, 'session', '{"adapter":"codex","vendor_version":"unverified","platform":"darwin-arm64","mode":"unverified","manifest_rev":"codex-1"}', 0, 2);
		INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
		VALUES ('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'turn_end', 'tested', 'fixture', 1),
		       ('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'reminder', 'tested', 'fixture', 1),
		       ('codex', '0.170.0', 'darwin-arm64', 'exec', 'codex-1', 'turn_end', 'tested', 'fixture', 1);
		INSERT INTO turn(principal_id, launch_ref, turn_ref, opened_hook_at_ms)
		VALUES (1, 'old-claude', 'yesterday', 1), (1, 'current-claude', 'start', 2),
		       (1, 'current-claude', 'end', 3), (1, 'current-claude', 'tomorrow', 4);
	`)
	day := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	now := day.Add(16 * time.Hour).In(time.FixedZone("KST", 9*60*60))
	nextDay := day.Add(24 * time.Hour)
	update(t, `
		INSERT INTO turn_end_notice(principal_id, adapter, launch_ref, turn_ref, at_ms)
		VALUES (1, 'claude', 'old-claude', 'yesterday', ?),
		       (1, 'claude', 'current-claude', 'start', ?),
		       (1, 'claude', 'current-claude', 'end', ?),
		       (1, 'claude', 'current-claude', 'tomorrow', ?)`,
		day.UnixMilli()-1, day.UnixMilli(), nextDay.UnixMilli()-1, nextDay.UnixMilli())

	check := func(t *testing.T, wantClaudeAvailable bool, wantTrustedFrom string) {
		t.Helper()
		adapters := []doctorAdapter{
			{InstallStatus: setup.InstallStatus{Adapter: "claude"}},
			{InstallStatus: setup.InstallStatus{Adapter: "codex"}},
		}
		if _, err := doctorStoreInfo(ctx, state, adapters, now); err != nil {
			t.Fatal(err)
		}
		raw := (LaunchConfig{}).doctorResult("", false, nil, adapters, nil).Data
		var got struct {
			Adapters []struct {
				Adapter       string
				TurnEndNotice *struct {
					Available                         *bool
					Date                              string
					TrustedFrom                       string `json:"trusted_from"`
					CommittedClaims                   *int   `json:"committed_claims"`
					ExtraModelRequestsPerContinuation int    `json:"extra_model_requests_per_continuation"`
				} `json:"turn_end_notice"`
			}
		}
		if err := json.Unmarshal(raw, &got); err != nil || len(got.Adapters) != 2 {
			t.Fatalf("doctor JSON: %s (%v)", raw, err)
		}
		for i, want := range []struct {
			adapter     string
			available   bool
			claims      int
			trustedFrom string
		}{{"claude", wantClaudeAvailable, 2, wantTrustedFrom}, {"codex", false, 0, ""}} {
			a := got.Adapters[i]
			n := a.TurnEndNotice
			if a.Adapter != want.adapter || n == nil || n.Available == nil || *n.Available != want.available ||
				n.Date != "20261119" || n.CommittedClaims == nil || *n.CommittedClaims != want.claims ||
				n.ExtraModelRequestsPerContinuation != 1 || n.TrustedFrom != want.trustedFrom {
				t.Errorf("doctor JSON: %s, want %s available=%t claims=%d trusted_from=%q", raw, want.adapter, want.available, want.claims, want.trustedFrom)
			}
		}
	}
	check(t, true, "")
	t.Run("inherited from another version", func(t *testing.T) {
		update(t, "UPDATE capability_evidence SET vendor_version = '2.1.328' WHERE vendor = 'claude' AND cap = 'turn_end'")
		defer update(t, "UPDATE capability_evidence SET vendor_version = '2.1.327' WHERE vendor = 'claude' AND cap = 'turn_end'")
		check(t, true, "2.1.328")
	})
	for _, test := range []struct {
		name, disable, restore string
	}{
		{"policy off", "UPDATE adapter_policy SET enabled = 0 WHERE adapter = 'claude'",
			"UPDATE adapter_policy SET enabled = 1 WHERE adapter = 'claude'"},
		{"every launch ended", "UPDATE adapter_launch SET ended_hook_at_ms = 3 WHERE principal_id = 1",
			"UPDATE adapter_launch SET ended_hook_at_ms = NULL WHERE principal_id = 1"},
		{"binding departed", "UPDATE binding SET state = 'departed' WHERE principal_id = 1",
			"UPDATE binding SET state = 'live' WHERE principal_id = 1"},
		{"wrong platform", "UPDATE capability_evidence SET platform = 'linux-amd64' WHERE vendor = 'claude' AND cap = 'turn_end'",
			"UPDATE capability_evidence SET platform = 'darwin-arm64' WHERE vendor = 'claude' AND cap = 'turn_end'"},
		{"wrong mode", "UPDATE capability_evidence SET mode = 'interactive' WHERE vendor = 'claude' AND cap = 'turn_end'",
			"UPDATE capability_evidence SET mode = 'print' WHERE vendor = 'claude' AND cap = 'turn_end'"},
		{"wrong manifest", "UPDATE capability_evidence SET manifest_rev = 'claude-2' WHERE vendor = 'claude' AND cap = 'turn_end'",
			"UPDATE capability_evidence SET manifest_rev = 'claude-1' WHERE vendor = 'claude' AND cap = 'turn_end'"},
		{"candidate", "UPDATE capability_evidence SET level = 'candidate' WHERE vendor = 'claude' AND cap = 'turn_end'",
			"UPDATE capability_evidence SET level = 'tested' WHERE vendor = 'claude' AND cap = 'turn_end'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			update(t, test.disable)
			defer update(t, test.restore)
			check(t, false, "")
		})
	}
}

func TestDoctorExplainsCodexSharedDaemonRouting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PATH", t.TempDir())
	cfg, _ := doctorFixture(t)
	config := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", config)
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	command := "/fixture/bin/crosshall hook codex SessionStart"
	pinned := "CROSSHALL_STATE_DIR='" + cfg.StateRoot + "' " + command
	manual := `CROSSHALL_STATE_DIR="/another state" /fixture/bin/crosshall hook codex Stop`
	for _, test := range []struct {
		name, route, detail string
		commands            map[string][]string
	}{
		{name: "absent"},
		{"legacy", "inherited", "use that environment", map[string][]string{"SessionStart": {command}}},
		{"pinned", "pinned", "pin the Crosshall state root", map[string][]string{"SessionStart": {pinned}}},
		{"mixed", "mixed", "mix state roots or routing styles", map[string][]string{"SessionStart": {command, pinned}}},
		{"pinned_and_uncertain", "uncertain", "uncertain state routing", map[string][]string{"SessionStart": {pinned}, "Stop": {manual}}},
		{"uncertain_only", "uncertain", "uncertain state routing", map[string][]string{"Stop": {manual}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			type handler struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if len(test.commands) > 0 {
				hooks := make(map[string][]map[string][]handler)
				for event, commands := range test.commands {
					handlers := make([]handler, 0, len(commands))
					for _, cmd := range commands {
						handlers = append(handlers, handler{"command", cmd, 1})
					}
					hooks[event] = []map[string][]handler{{"hooks": handlers}}
				}
				configJSON := map[string]any{"hooks": hooks}
				raw, err := json.Marshal(configJSON)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(config, "hooks.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			result := cfg.Doctor(t.Context())
			if result.Status != core.ExitOK {
				t.Fatalf("routing changed doctor exit to %v: %s", result.Status, result.Data)
			}
			var data struct {
				Adapters []struct {
					Adapter string `json:"adapter"`
					Route   string `json:"codex_hook_state_routing"`
				} `json:"adapters"`
				Warnings []string `json:"warnings"`
			}
			if err := json.Unmarshal(result.Data, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Adapters) != 3 || data.Adapters[0].Route != "" || data.Adapters[1].Adapter != "codex" ||
				data.Adapters[1].Route != test.route || data.Adapters[2].Adapter != "agy" || data.Adapters[2].Route != "" {
				t.Errorf("doctor adapters = %+v, want Codex route %q", data.Adapters, test.route)
			}
			if test.route == "" {
				if len(data.Warnings) != 0 {
					t.Errorf("warnings without Codex hooks: %q", data.Warnings)
				}
			} else if len(data.Warnings) != 1 || !strings.Contains(data.Warnings[0], "can run in a shared managed daemon") || !strings.Contains(data.Warnings[0], test.detail) {
				t.Errorf("warnings = %q, want conditional daemon and %q", data.Warnings, test.detail)
			}
		})
	}
}

func TestDoctorReportsAdapterEvidenceAndTodayCounters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PATH", t.TempDir())
	cfg, _ := doctorFixture(t)
	db, err := store.Open(t.Context(), cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms)
			VALUES ('claude', 1, 1, 7, 18);
			INSERT INTO channel(id, name, created_at_ms) VALUES (1, 'checkout', 1);
			INSERT INTO principal(id, kind, vendor, created_at_ms)
			VALUES (1, 'session', 'claude', 1), (2, 'session', 'claude', 1);
			INSERT INTO binding(principal_id, channel_id, alias, state, joined_at_ms)
			VALUES (1, 1, 'private-alias', 'live', 1), (2, 1, 'departed-alias', 'departed', 1);
			INSERT INTO adapter_launch(principal_id, launch_ref, profile, verified, started_hook_at_ms) VALUES
			  (1, 'old-launch', '{"adapter":"claude","vendor_version":"unverified","mode":"unverified"}', 0, 2),
			  (1, 'live-launch', '{"adapter":"claude","vendor_version":"2.1.327","mode":"unverified"}', 1, 4),
			  (2, 'departed-launch', '{"adapter":"claude","vendor_version":"unverified","mode":"unverified"}', 0, 3);`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hookDir := filepath.Join(cfg.Rendezvous, "hooks")
	if err := os.Mkdir(hookDir, 0700); err != nil {
		t.Fatal(err)
	}
	records := "claude Stop clean 4\nclaude SessionStart rpc 10\nclaude Stop clean 6\n" +
		"claude UserPromptSubmit deadline 145\nclaude Stop deadline 150\n" +
		"claude Report secret-outcome 990\nclaude Stop clean 500"
	today := time.Now().UTC()
	if err := os.WriteFile(filepath.Join(hookDir, today.AddDate(0, 0, -1).Format("20060102")), []byte("codex Stop deadline 300\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, today.Format("20060102")), []byte(records), 0600); err != nil {
		t.Fatal(err)
	}
	beforeImport := cfg.Doctor(t.Context())
	checkDoctor(t, beforeImport, core.ExitOK, true, true, true, true, true)
	if err := db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
			VALUES ('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'enroll', 'tested', 'private-evidence-reference', 19),
			       ('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'reminder', 'unavailable', 'private-evidence-reference', 19);`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result := cfg.Doctor(t.Context())
	checkDoctor(t, result, core.ExitOK, true, true, true, true, true)
	type diagnostics struct {
		Adapters []struct {
			Adapter  string
			Policy   *store.AdapterPolicy
			Reminder *reminderDiagnostic
			Evidence []struct {
				Version    string
				Platform   string
				Mode       string
				Capability string
				Level      string
			}
			UnverifiedLaunches *int `json:"unverified_launches"`
			Hooks              *struct {
				Date            string
				Counts          map[string]int
				P95MS           int             `json:"p95_ms"`
				RecordedStarts  int             `json:"recorded_starts"`
				HelperTimeouts  int             `json:"helper_timeouts"`
				MissedDeadlines int             `json:"missed_deadlines"`
				VendorTimeouts  json.RawMessage `json:"vendor_timeouts"`
			}
		}
	}
	var before, data diagnostics
	if err := json.Unmarshal(beforeImport.Data, &before); err != nil || len(before.Adapters) != 3 {
		t.Fatalf("adapter diagnostics before evidence import: %s (%v)", beforeImport.Data, err)
	}
	if err := json.Unmarshal(result.Data, &data); err != nil || len(data.Adapters) != 3 {
		t.Fatalf("adapter diagnostics: %s (%v)", result.Data, err)
	}
	for i, adapter := range data.Adapters {
		for _, reminder := range []*reminderDiagnostic{before.Adapters[i].Reminder, adapter.Reminder} {
			if reminder == nil || reminder.Available == nil || *reminder.Available || reminder.ReleaseState != "parked" ||
				reminder.Reason == "" || reminder.Provenance == "" || reminder.ExtraModelRequests != 1 ||
				len(reminder.Evidence) != 1 || reminder.Evidence[0].Level != "unknown" ||
				reminder.Evidence[0].Version != "" || reminder.Evidence[0].Mode != "" {
				t.Errorf("%s reminder status without a cached build: %+v", adapter.Adapter, reminder)
			}
		}
	}
	claude, codex := data.Adapters[0], data.Adapters[1]
	if claude.Adapter != "claude" || claude.Policy == nil || !claude.Policy.Enabled || !claude.Policy.Reminder || claude.Policy.Epoch != 7 {
		t.Errorf("Claude policy: %+v", claude)
	}
	if len(claude.Evidence) != 2 {
		t.Errorf("Claude evidence: %+v", claude.Evidence)
	} else if e := claude.Evidence[0]; e.Version != "2.1.327" || e.Platform != "darwin-arm64" || e.Mode != "print" || e.Capability != "enroll" || e.Level != "tested" {
		t.Errorf("Claude evidence: %+v", e)
	} else if e := claude.Evidence[1]; e.Capability != "reminder" || e.Level != "unavailable" {
		t.Errorf("Claude reminder evidence: %+v", e)
	}
	if n := claude.UnverifiedLaunches; n == nil || *n != 1 {
		t.Errorf("Claude unverified-launch count: %+v", claude)
	}
	if h := claude.Hooks; h == nil || h.Date != today.Format("20060102") ||
		len(h.Counts) != 3 || h.Counts["clean"] != 2 || h.Counts["rpc"] != 1 || h.Counts["deadline"] != 2 || h.P95MS != 150 ||
		h.RecordedStarts != 5 || h.HelperTimeouts != 2 || h.MissedDeadlines != 1 || string(h.VendorTimeouts) != "null" {
		t.Errorf("Claude hooks: %+v", h)
	}
	if codex.Adapter != "codex" || codex.Policy == nil || codex.Policy.Enabled || codex.Policy.Epoch != 0 || codex.Hooks != nil {
		t.Errorf("Codex policy and counters: %+v", codex)
	}
	for _, private := range []string{"secret-outcome", "private-evidence-reference", "private-alias", "live-launch"} {
		if strings.Contains(string(result.Data), private) {
			t.Errorf("diagnostics leaked %q", private)
		}
	}
}

func TestDoctorShowsOnlyReminderEvidenceForInstalledBuild(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	claudeHome := filepath.Join(home, ".claude")
	if err := os.Mkdir(claudeHome, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '9.1.42 (Claude Code)\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := doctorFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	svc := setup.Service{
		StateRoot: cfg.StateRoot, Executable: executable,
		SetPolicy: func(context.Context, string, bool) error { return nil },
	}
	if err := svc.Run(t.Context(), setup.Request{
		Action: "setup", Input: strings.NewReader("y\nn\nn\n"), Output: io.Discard,
	}); err != nil {
		t.Fatal(err)
	}
	readReminder := func() *reminderDiagnostic {
		t.Helper()
		r := cfg.Doctor(t.Context())
		checkDoctor(t, r, core.ExitOK, true, true, true, true, true)
		var data struct {
			Adapters []struct {
				Adapter, Version string
				Reminder         *reminderDiagnostic
			}
		}
		if err := json.Unmarshal(r.Data, &data); err != nil || len(data.Adapters) != 3 {
			t.Fatalf("adapter diagnostics: %s (%v)", r.Data, err)
		}
		claude := data.Adapters[0]
		if claude.Adapter != "claude" || claude.Version != "9.1.42" || claude.Reminder == nil {
			t.Fatalf("cached Claude build and reminder: %s", r.Data)
		}
		if r := claude.Reminder; r.Available == nil || *r.Available || r.ReleaseState != "parked" || r.ExtraModelRequests != 1 {
			t.Fatalf("Claude release reminder status: %+v", r)
		}
		return claude.Reminder
	}
	if r := readReminder(); len(r.Evidence) != 1 || r.Evidence[0].Level != "unknown" || r.Evidence[0].Mode != "" {
		t.Fatalf("Claude reminder before evidence import: %+v", r)
	}
	db, err := store.Open(t.Context(), cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if err := db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
			VALUES ('claude', '9.1.42', ?1, 'print', 'claude-3', 'reminder', 'candidate', 'matching', 19),
			       ('claude', '2.1.327', ?1, 'print', 'claude-3', 'reminder', 'unavailable', 'other-version', 19),
			       ('claude', '9.1.42', 'other-platform', 'print', 'claude-3', 'reminder', 'unavailable', 'other-platform', 19),
			       ('claude', '9.1.42', ?1, 'print', 'claude-1', 'reminder', 'unavailable', 'other-revision', 19),
			       ('claude', '9.1.42', ?1, 'interactive', 'claude-3', 'enroll', 'tested', 'other-capability', 19);`, platform)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := readReminder(); len(r.Evidence) != 1 || r.Evidence[0].Level != "candidate" ||
		r.Evidence[0].Version != "9.1.42" || r.Evidence[0].Platform != platform ||
		r.Evidence[0].Mode != "print" || r.Evidence[0].ManifestRev != "claude-3" {
		t.Fatalf("Claude reminder after evidence import: %+v", r)
	}
}

func TestDoctorReportsPublishStatePerMemberAndCounters(t *testing.T) {
	cfg, _ := doctorFixture(t)
	ctx := t.Context()
	db, err := store.Open(ctx, cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	day := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	now := day.Add(10 * time.Hour)
	at := now.Add(-time.Minute).UnixMilli()
	if err := db.Update(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `
		INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms) VALUES ('claude', 1, 0, 1, 1);
		INSERT INTO channel(id, name, created_at_ms) VALUES (1, 'dev', 1);
		INSERT INTO principal(id, kind, vendor, created_at_ms) VALUES (1, 'session', 'claude', 1), (2, 'session', 'claude', 1);
		INSERT INTO binding(id, principal_id, channel_id, alias, state, joined_at_ms)
		VALUES (1, 1, 1, 'absorbed', 'live', 1), (2, 2, 1, 'quiet', 'live', 1);
		UPDATE binding SET publish_quiet = 1 WHERE id = 2;
		INSERT INTO adapter_launch(principal_id, launch_ref, profile, verified, started_hook_at_ms) VALUES
		  (1, 'one', '{"adapter":"claude","vendor_version":"2.1.0","platform":"darwin-arm64","mode":"default","manifest_rev":"r1"}', 1, 1),
		  (2, 'two', '{"adapter":"claude","vendor_version":"2.1.0","platform":"darwin-arm64","mode":"default","manifest_rev":"r1"}', 1, 1);
		INSERT INTO publish_route(binding_id, principal_id, generation, adapter, session_ref, launch_ref,
			launch_verified, policy_epoch, mode, socket_path, watermark_message_id, state, registered_at_ms, updated_at_ms)
		VALUES (1, 1, 1, 'claude', 's1', 'one', 1, 1, 'default', '/tmp/cc-socks/1.sock', 0, 'active', 1, 1),
		       (2, 2, 1, 'claude', 's2', 'two', 1, 1, 'default', '/tmp/cc-socks/2.sock', 0, 'active', 1, 1);
		INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
			policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation)
		VALUES (1, 1, 1, 1, 'claude', 's1', 'one', 1, 11, 1, 'submitted', ?1, ?1, ?1, ?1, 'own_turn'),
		       (1, 1, 1, 1, 'claude', 's1', 'one', 1, 12, 1, 'submitted', ?1, ?1, ?1, ?1, 'absorbed'),
		       (1, 1, 1, 1, 'claude', 's1', 'one', 1, 13, 1, 'not_submitted', ?1, ?1, ?1, NULL, NULL),
		       (1, 1, 1, 1, 'claude', 's1', 'one', 1, 14, 1, 'submitted', ?2, ?2, ?2, ?2, 'own_turn'),
		       (1, 1, 1, 1, 'claude', 's1', 'one', 1, 15, 1, 'submitted', ?2, ?2, ?2, ?2, 'unknown');
		INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
			policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation,
			unknown_cause)
		VALUES (1, 1, 1, 1, 'claude', 's1', 'one', 1, 16, 1, 'submitted', ?1, ?1, ?1, ?1, 'unknown', 'open_placeholder'),
		       (1, 1, 1, 1, 'claude', 's1', 'one', 1, 17, 1, 'submitted', ?1, ?1, ?1, ?1, 'unknown', 'open_placeholder'),
		       (1, 1, 1, 1, 'claude', 's1', 'one', 1, 18, 1, 'submitted', ?1, ?1, ?1, ?1, 'unknown', 'no_turn_but_facts'),
		       (1, 1, 1, 1, 'claude', 's1', 'one', 1, 19, 1, 'submitted', ?1, ?1, ?1, ?1, 'unknown', NULL),
		       (1, 1, 1, 1, 'claude', 's1', 'one', 1, 20, 1, 'submitted', ?2, ?2, ?2, ?2, 'unknown', 'unfinished_turn');
		INSERT INTO publish_suspension (binding_id, launch_ref, reason, notice, at_ms, until_ms)
		VALUES (1, 'one', 'absorbed', 2, ?1, ?3)`,
			at, day.UnixMilli()-1, at+30*60*1000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	adapters := []doctorAdapter{{InstallStatus: setup.InstallStatus{Adapter: "claude"}}}
	if _, err := doctorStoreInfo(ctx, state, adapters, now); err != nil {
		t.Fatal(err)
	}
	raw := (LaunchConfig{}).doctorResult("", false, nil, adapters, nil).Data
	var got struct {
		Adapters []struct {
			Publish *store.DoctorPublishState
		}
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Adapters) != 1 || got.Adapters[0].Publish == nil {
		t.Fatalf("doctor JSON: %s (%v)", raw, err)
	}
	p := got.Adapters[0].Publish
	if want := (store.DoctorPublishCounters{Date: "20261121", Submitted: 6, NotSubmitted: 1, OwnTurn: 1, Absorbed: 1, Unknown: 4,
		UnknownCauses: store.DoctorUnknownCauses{OpenPlaceholder: 2, NoTurnButFacts: 1, NotRecorded: 1}}); p.Counters != want {
		t.Errorf("counters %+v, want %+v (the previous day's notices are not counted)", p.Counters, want)
	}
	if len(p.Members) != 2 {
		t.Fatalf("members %+v", p.Members)
	}
	if m := p.Members[0]; m.Alias != "absorbed" || m.State.State != "suspended" || m.State.Reason != "notice 2 absorbed" ||
		m.State.AtMS != at || m.State.UntilMS != at+30*60*1000 || m.State.Lift == "" {
		t.Errorf("absorbed member %+v", m)
	}
	if m := p.Members[1]; m.Alias != "quiet" || m.State.State != "off" || m.State.Reason != "turned off by the member" {
		t.Errorf("quiet member %+v", m)
	}
}
