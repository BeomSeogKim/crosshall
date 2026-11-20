package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/setuptest"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func TestStatusFreshDefaultsAreReadOnly(t *testing.T) {
	f := setuptest.New(t, "claude")
	probe := filepath.Join(f.Home, "vendor-ran")
	f.CheckTargets(t)
	if err := os.WriteFile(filepath.Join(f.Bin, "claude"), []byte("#!/bin/sh\nprintf ran >"+shellQuote(probe)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	statuses := readAdapterStatuses(t, f, "")
	if len(statuses) != 3 {
		t.Fatalf("adapter statuses: %+v", statuses)
	}
	for i, want := range []struct{ adapter, cli, path string }{
		{"claude", "found", ".claude/settings.json"},
		{"codex", "missing", ".codex/hooks.json"},
		{"agy", "missing", ".gemini/config/hooks.json"},
	} {
		got := statuses[i]
		if got.Adapter != want.adapter || got.CLI != want.cli || !got.HookAdapter ||
			got.PathSource != "default" || got.Path != filepath.Join(f.Home, want.path) || got.PathError != "" ||
			got.Install != "none" || got.Owned || got.Policy != "default" || got.Footprint != "none" || got.Pending != nil {
			t.Errorf("%s status: %+v", want.adapter, got)
		}
	}
	for _, path := range []string{f.State, filepath.Join(f.Home, ".claude"), filepath.Join(f.Home, ".codex"), filepath.Join(f.Home, ".gemini"), probe} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("status created %s or could not verify its absence: %v", path, err)
		}
	}
}

func TestStatusRecognizesHookFootprintsWithoutVendorExecutables(t *testing.T) {
	f := setuptest.New(t)
	executable := filepath.Join(f.Bin, "crosshall")
	for _, spec := range hook.InstallSpecs() {
		switch spec.Adapter {
		case "claude":
			old := filepath.Join(f.Root, "old", "crosshall")
			writeStatusHooks(t, f, spec, old, 1)
		case "codex":
			writeStatusHooks(t, f, spec, executable, len(spec.Events))
		}
	}
	statuses := readAdapterStatuses(t, f, executable)
	for i, want := range []string{"drift", "installed"} {
		got := statuses[i]
		if got.CLI != "missing" || got.Owned || got.Install != want || got.Footprint != "present" {
			t.Errorf("%s status: %+v", got.Adapter, got)
		}
	}
}

func TestStatusRecognizesOnlyCrosshallHookCommands(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   string
		args      []string
		footprint string
		install   string
	}{
		{"plain Claude shell", "crosshall hook claude Stop", nil, "present", "drift"},
		{"quoted Claude shell", shellQuote("/old user's bin/crosshall") + " hook claude Stop", nil, "present", "drift"},
		{"double-quoted Claude path", `"/tmp/owner tools/crosshall" hook claude Stop`, nil, "present", "drift"},
		{"double-quoted unrelated path", `"/tmp/owner tools/status-tracker" hook claude Stop`, nil, "none", "none"},
		{"double-quoted path with expansion", `"/tmp/$tools/crosshall" hook claude Stop`, nil, "none", "none"},
		{"Claude argv", "/old bin/crosshall", []string{"hook", "claude", "Stop"}, "present", "drift"},
		{"unrelated Claude argv", "/opt/vendor/bin/status-tracker", []string{"hook", "claude", "Stop"}, "none", "none"},
		{"unrelated Claude shell", "/opt/vendor/bin/status-tracker hook claude Stop", nil, "none", "none"},
		{"unquoted shell path with spaces", "/old bin/crosshall hook claude Stop", nil, "none", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setuptest.New(t)
			handler := map[string]any{"type": "command", "command": tc.command}
			if tc.args != nil {
				handler["args"] = tc.args
			}
			body, err := json.Marshal(map[string]any{"hooks": map[string]any{
				"Stop": []any{map[string]any{"hooks": []any{handler}}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.Home, ".claude", "settings.json")
			f.CheckTargets(t)
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			got := readAdapterStatuses(t, f, filepath.Join(f.Bin, "crosshall"))[0]
			if got.Footprint != tc.footprint || got.Install != tc.install || got.Owned {
				t.Fatalf("status: %+v", got)
			}
		})
	}
}

func TestStatusDistinguishesPolicyAndJournalHistory(t *testing.T) {
	f := setuptest.New(t, "claude", "codex")
	j := emptyJournal()
	j.Builds["claude"] = build{Version: "2.1.327"} // Build history remains after uninstall.
	writeStatusJournal(t, f, j)
	statuses := readAdapterStatuses(t, f, "")
	if claude := statuses[0]; claude.Owned || claude.Install != "none" || claude.Footprint != "present" || claude.Policy != "default" {
		t.Errorf("post-uninstall history: %+v", claude)
	}
	if codex := statuses[1]; codex.Footprint != "none" || codex.Policy != "default" {
		t.Errorf("no Codex history: %+v", codex)
	}

	f.CheckTargets(t)
	db, err := store.Open(t.Context(), f.State)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f.CheckTargets(t)
	if err := db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms)
			VALUES ('claude', 1, 0, 2, 42), ('codex', 0, 0, 3, 43)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	statuses = readAdapterStatuses(t, f, "")
	for i, want := range []string{"on", "off"} {
		got := statuses[i]
		if got.Policy != want || got.Owned || got.Install != "none" || got.Footprint != "present" {
			t.Errorf("%s explicit policy: %+v", got.Adapter, got)
		}
	}
	var output bytes.Buffer
	if err := RenderStatus(&output, statuses, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Claude Code — CLI found, hooks none, policy on", "Codex — CLI found, hooks none, policy off"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q in %s", want, output.String())
		}
	}
}

func TestStatusKeepsRecordedPathAndOriginalPendingScope(t *testing.T) {
	f := setuptest.New(t, "claude", "codex")
	recorded := filepath.Join(f.Home, "original-claude", "settings.json")
	j := emptyJournal()
	j.Files["claude"] = ownedFile{Path: recorded, Entries: claudeRules(false)}
	j.Pending = &operation{
		Action: "setup", Adapters: []string{"claude", "codex"},
		Edits: []edit{{Adapter: "claude", Path: recorded, Add: claudeRules(false)}},
	}
	writeStatusJournal(t, f, j)
	statuses := readAdapterStatuses(t, f, "")
	if got := statuses[0]; !got.Owned || got.RecordedPath != recorded || got.Path == recorded || got.Install != "drift" {
		t.Errorf("Claude recorded path: %+v", got)
	}
	for _, got := range statuses[:2] {
		if p := got.Pending; p == nil || p.Action != "setup" || !slices.Equal(p.Adapters, j.Pending.Adapters) ||
			!p.Rules.Read || p.Rules.Send || got.Footprint != "present" {
			t.Errorf("%s pending status: %+v", got.Adapter, got)
		}
	}
	var output bytes.Buffer
	if err := RenderStatus(&output, statuses, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Claude Code — CLI found, hooks drift, policy off, footprint present",
		"configuration " + filepath.Join(f.Home, ".claude", "settings.json"),
		"Interrupted setup for claude, codex (Claude read rules true, send rules false)",
		"Antigravity — CLI missing, hooks none, policy off, footprint present",
	} {
		if !strings.Contains(strings.ReplaceAll(output.String(), "\"", ""), want) {
			t.Errorf("missing %q in %s", want, output.String())
		}
	}
	output.Reset()
	if err := RenderStatus(&output, statuses, true); err != nil {
		t.Fatal(err)
	}
	var encoded struct {
		Version  int             `json:"v"`
		OK       bool            `json:"ok"`
		Kind     string          `json:"kind"`
		Adapters []AdapterStatus `json:"adapters"`
	}
	if err := json.Unmarshal(output.Bytes(), &encoded); err != nil || encoded.Version != 1 ||
		!encoded.OK || encoded.Kind != "setup status" || len(encoded.Adapters) != 3 ||
		encoded.Adapters[0].Policy != "default" || encoded.Adapters[0].RecordedPath != recorded || encoded.Adapters[0].Pending == nil ||
		!encoded.Adapters[0].Pending.Rules.Read || !encoded.Adapters[2].HookAdapter {
		t.Fatalf("status JSON: %s (%v)", output.String(), err)
	}
}

func TestStatusFailsClosedForUnsafeInputs(t *testing.T) {
	t.Run("override and linked configuration", func(t *testing.T) {
		f := setuptest.New(t, "claude", "codex")
		f.CheckTargets(t)
		if err := os.Symlink(filepath.Join(f.Outside, ".claude"), filepath.Join(f.Home, ".claude")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CODEX_HOME", "")
		for _, got := range readAdapterStatuses(t, f, "")[:2] {
			if got.Install != unknown || got.Footprint != unknown || got.PathError == "" {
				t.Errorf("%s unsafe target: %+v", got.Adapter, got)
			}
		}
	})
	for _, name := range []string{journalName, "store.sqlite3", "store.sqlite3-wal"} {
		t.Run(name, func(t *testing.T) {
			f := setuptest.New(t, "claude", "codex")
			f.CheckTargets(t)
			if err := os.Mkdir(f.State, 0700); err != nil {
				t.Fatal(err)
			}
			f.CheckTargets(t)
			path := filepath.Join(f.State, name)
			const marker = "invalid status state fixture\n"
			if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
			for _, got := range readAdapterStatuses(t, f, "")[:2] {
				if got.Install != "none" || got.Footprint != unknown ||
					(name != journalName && got.Policy != unknown) {
					t.Errorf("%s unreliable state: %+v", got.Adapter, got)
				}
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != marker {
				t.Errorf("status changed %s: %q (%v)", path, after, err)
			}
		})
	}
}

func readAdapterStatuses(t *testing.T, f setuptest.Fixture, executable string) []AdapterStatus {
	t.Helper()
	statuses, err := (Service{StateRoot: f.State, Executable: executable}).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return statuses
}

func writeStatusHooks(t *testing.T, f setuptest.Fixture, spec hook.InstallSpec, executable string, count int) {
	t.Helper()
	target, err := resolveConfigPath(spec)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := hookEntries(spec, executable, f.State)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := addEntries(nil, entries[:count])
	if err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := os.Mkdir(filepath.Dir(target.Path), 0700); err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := os.WriteFile(target.Path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeStatusJournal(t *testing.T, f setuptest.Fixture, j journal) {
	t.Helper()
	f.CheckTargets(t)
	state, lock, err := (Service{StateRoot: f.State}).lockState()
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	defer lock.Close()
	f.CheckTargets(t)
	if err := writeJournal(state, j); err != nil {
		t.Fatal(err)
	}
}
