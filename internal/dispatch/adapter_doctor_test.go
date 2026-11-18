package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

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
			VALUES ('claude', 1, 0, 7, 18);
			INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
			VALUES ('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'enroll', 'tested', 'private-evidence-reference', 19);
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
		"claude Report secret-outcome 990\nclaude Stop clean 500"
	if err := os.WriteFile(filepath.Join(hookDir, time.Now().UTC().Format("20060102")), []byte(records), 0600); err != nil {
		t.Fatal(err)
	}
	result := cfg.Doctor(t.Context())
	checkDoctor(t, result, core.ExitOK, true, true, true, true, true)
	var data struct {
		Adapters []struct {
			Adapter  string
			Policy   *store.AdapterPolicy
			Evidence []struct {
				Version    string
				Platform   string
				Mode       string
				Capability string
				Level      string
			}
			UnverifiedLaunches *int `json:"unverified_launches"`
			Hooks              *struct {
				Counts map[string]int
				P95MS  int `json:"p95_ms"`
			}
		}
	}
	if err := json.Unmarshal(result.Data, &data); err != nil || len(data.Adapters) != 2 {
		t.Fatalf("adapter diagnostics: %s (%v)", result.Data, err)
	}
	claude, codex := data.Adapters[0], data.Adapters[1]
	if claude.Adapter != "claude" || claude.Policy == nil || !claude.Policy.Enabled || claude.Policy.Epoch != 7 {
		t.Errorf("Claude policy: %+v", claude)
	}
	if len(claude.Evidence) != 1 {
		t.Errorf("Claude evidence: %+v", claude.Evidence)
	} else if e := claude.Evidence[0]; e.Version != "2.1.327" || e.Platform != "darwin-arm64" || e.Mode != "print" || e.Capability != "enroll" || e.Level != "tested" {
		t.Errorf("Claude evidence: %+v", e)
	}
	if n := claude.UnverifiedLaunches; n == nil || *n != 1 {
		t.Errorf("Claude unverified-launch count: %+v", claude)
	}
	if h := claude.Hooks; h == nil || len(h.Counts) != 2 || h.Counts["clean"] != 2 || h.Counts["rpc"] != 1 || h.P95MS != 10 {
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
