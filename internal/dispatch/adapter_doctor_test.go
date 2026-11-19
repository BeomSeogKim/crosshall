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
	if err := json.Unmarshal(beforeImport.Data, &before); err != nil || len(before.Adapters) != 2 {
		t.Fatalf("adapter diagnostics before evidence import: %s (%v)", beforeImport.Data, err)
	}
	if err := json.Unmarshal(result.Data, &data); err != nil || len(data.Adapters) != 2 {
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
		if err := json.Unmarshal(r.Data, &data); err != nil || len(data.Adapters) != 2 {
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
			VALUES ('claude', '9.1.42', ?1, 'print', 'claude-1', 'reminder', 'candidate', 'matching', 19),
			       ('claude', '2.1.327', ?1, 'print', 'claude-1', 'reminder', 'unavailable', 'other-version', 19),
			       ('claude', '9.1.42', 'other-platform', 'print', 'claude-1', 'reminder', 'unavailable', 'other-platform', 19),
			       ('claude', '9.1.42', ?1, 'print', 'claude-0', 'reminder', 'unavailable', 'other-revision', 19),
			       ('claude', '9.1.42', ?1, 'interactive', 'claude-1', 'enroll', 'tested', 'other-capability', 19);`, platform)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := readReminder(); len(r.Evidence) != 1 || r.Evidence[0].Level != "candidate" ||
		r.Evidence[0].Version != "9.1.42" || r.Evidence[0].Platform != platform ||
		r.Evidence[0].Mode != "print" || r.Evidence[0].ManifestRev != "claude-1" {
		t.Fatalf("Claude reminder after evidence import: %+v", r)
	}
}
