package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func TestSetupAndInspectReportParkedReminderWithBaseHooks(t *testing.T) {
	service, claudePath := setupFixture(t)
	service.Executable = filepath.Join(os.Getenv("HOME"), "installed binaries", "crosshall")
	codexHome := os.Getenv("CODEX_HOME")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(os.Getenv("PATH"), "codex")
	if err := os.WriteFile(codex, []byte("#!/bin/sh\nprintf 'codex-cli 0.170.0\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := service.Run(t.Context(), Request{Action: "setup", DryRun: true, JSON: true, Output: &out}); err != nil {
		t.Fatal(err)
	}
	var preview plan
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Applied || len(preview.Files) != 5 {
		t.Fatalf("unexpected preview: %+v", preview)
	}
	baseEvents := map[string][]string{
		"claude": {"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "Notification", "SessionEnd"},
		"codex":  {"SessionStart", "UserPromptSubmit", "Stop", "PermissionRequest", "SessionEnd"},
	}
	previewed := make(map[string]PlanFile)
	for _, f := range preview.Files {
		if f.Skill != "" {
			continue
		}
		if f.AddText != "" {
			if f.Adapter != "codex" || f.Reminder != nil {
				t.Errorf("unexpected command rules plan: %v", f.Path)
			}
			continue
		}
		var events []string
		for _, e := range f.Add {
			if e.Event != "" {
				events = append(events, e.Event)
			}
		}
		if !slices.Equal(events, baseEvents[f.Adapter]) {
			t.Errorf("%s preview events = %v", f.Adapter, events)
		}
		if f.Reminder == nil || f.Reminder.ReleaseState != "parked" || f.Reminder.Available || f.Reminder.ExtraModelRequests != 1 ||
			f.Reminder.Reason == "" || f.Reminder.Provenance == "" {
			t.Fatalf("%s reminder = %+v", f.Adapter, f.Reminder)
		}
		previewed[f.Adapter] = f
	}
	for _, path := range []string{claudePath, filepath.Join(codexHome, "hooks.json"), service.StateRoot} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("dry-run changed %s: %v", path, err)
		}
	}

	// Doctor reads the build the install recorded. The install's own result
	// comes from the same version probe; the preview ran its own, which can
	// time out under load, so it is compared only when both read one version.
	var installed, result bytes.Buffer
	if err := service.Run(t.Context(), Request{Action: "setup", JSON: true, Verbose: true, Input: strings.NewReader("y\nn\nn\n"),
		Output: &result, Prompt: &installed}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(installed.String(), "Reply reminder parked (no opt-in)") != 2 ||
		strings.Count(installed.String(), "the reminder adds 1 model request") != 2 {
		t.Fatalf("setup omitted the availability or cost: %s", installed.String())
	}
	var applied plan
	if err := json.Unmarshal(result.Bytes(), &applied); err != nil || !applied.Applied {
		t.Fatalf("install result %s: %v", result.String(), err)
	}
	reminders := make(map[string]reminderStatus)
	for _, f := range applied.Files {
		if f.Reminder == nil {
			continue
		}
		reminders[f.Adapter] = *f.Reminder
		if p := previewed[f.Adapter]; p.Version == f.Version && !reflect.DeepEqual(*p.Reminder, *f.Reminder) {
			t.Errorf("%s preview reminder = %+v, install = %+v", f.Adapter, *p.Reminder, *f.Reminder)
		}
	}
	if len(reminders) != 2 {
		t.Fatalf("install result reminders: %+v", reminders)
	}
	for _, status := range Inspect(service.StateRoot) {
		if want, ok := reminders[status.Adapter]; ok && !reflect.DeepEqual(status.Reminder, want) {
			t.Errorf("%s doctor reminder = %+v, setup = %+v", status.Adapter, status.Reminder, want)
		}
		delete(reminders, status.Adapter)
	}
	if len(reminders) != 0 {
		t.Fatalf("missing doctor reminder statuses: %+v", reminders)
	}
	for _, path := range []string{claudePath, filepath.Join(codexHome, "hooks.json")} {
		contents, err := os.ReadFile(path)
		if err != nil || bytes.Contains(contents, []byte("PostToolUse")) {
			t.Fatalf("installed config %s: %v, %s", path, err, contents)
		}
	}
}

func TestReminderReasonKeepsMeasurementsScoped(t *testing.T) {
	claude := releaseReminderStatus("claude", "2.1.327", "darwin-arm64", "claude-1", nil).Evidence[0]
	codex := releaseReminderStatus("codex", "0.170.0", "darwin-arm64", "codex-1", nil).Evidence[0]
	if claude.Level != "unavailable" || claude.Mode != "print" || !strings.Contains(claude.Reason, "originating tool-call ID") ||
		codex.Level != "unavailable" || codex.Mode != "exec" || !strings.Contains(codex.Reason, "every Bash call") {
		t.Fatalf("measured profiles: %+v, %+v", claude, codex)
	}
	for _, profile := range []reminderStatus{
		releaseReminderStatus("claude", "unknown", "", "", nil),
		releaseReminderStatus("claude", "2.1.327", "linux-amd64", "claude-1", nil),
		releaseReminderStatus("claude", "2.1.327", "darwin-arm64", "claude-2", nil),
		releaseReminderStatus("codex", "0.171.0", "darwin-arm64", "codex-1", nil),
	} {
		if profile.Available || !slices.Equal(profile.Evidence, []reminderEvidence{{Level: "unknown"}}) {
			t.Fatalf("unmeasured profile: %+v", profile)
		}
	}
	candidate := store.DoctorEvidence{
		Version: claude.Version, Platform: claude.Platform, ManifestRev: claude.ManifestRev,
		Mode: "interactive", Capability: "reminder", Level: "candidate",
	}
	status := releaseReminderStatus("claude", claude.Version, claude.Platform, claude.ManifestRev, []store.DoctorEvidence{candidate})
	if len(status.Evidence) != 2 || status.Evidence[0].Level != "candidate" || status.Evidence[0].Mode != "interactive" || status.Evidence[1] != claude {
		t.Fatalf("mode-specific evidence: %+v", status.Evidence)
	}
	candidate.Mode = "print"
	status = releaseReminderStatus("claude", claude.Version, claude.Platform, claude.ManifestRev, []store.DoctorEvidence{candidate})
	if status.Available || len(status.Evidence) != 1 || status.Evidence[0].Level != "candidate" || status.Evidence[0].Reason != "" {
		t.Fatalf("candidate did not supersede the bundled observation: %+v", status)
	}
}

func TestSetupShowsCandidateForUnmeasuredBuildAfterImport(t *testing.T) {
	service, _ := setupFixture(t)
	const version = "8.9.10"
	vendor := filepath.Join(os.Getenv("PATH"), "claude")
	if err := os.WriteFile(vendor, []byte("#!/bin/sh\nprintf '8.9.10 (Claude Code)\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	assertEvidence := func(level, text string) {
		t.Helper()
		preview, err := runSetupTest(t, service, "setup", "", true)
		if err != nil || !strings.Contains(preview, "Reply reminder parked (no opt-in)") ||
			!strings.Contains(preview, text) || strings.Contains(preview, "unavailable") {
			t.Fatalf("setup text %s: %s (%v)", level, preview, err)
		}
		var out bytes.Buffer
		if err := service.Run(t.Context(), Request{Action: "setup", JSON: true, DryRun: true, Output: &out}); err != nil {
			t.Fatal(err)
		}
		var p plan
		if err := json.Unmarshal(out.Bytes(), &p); err != nil || len(p.Files) != 2 {
			t.Fatalf("setup JSON %s: %s (%v)", level, out.Bytes(), err)
		}
		s := p.Files[0].Reminder
		if s == nil || s.ReleaseState != "parked" || s.Available || len(s.Evidence) != 1 || s.Evidence[0].Level != level {
			t.Fatalf("reminder evidence %s: %+v", level, s)
		}
	}
	assertEvidence("unknown", "Reminder evidence unknown")
	if err := os.Mkdir(service.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), service.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if err := db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO capability_evidence
			(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
			VALUES ('claude', ?, ?, 'print', 'claude-3', 'reminder', 'candidate', 'fixture', 1)`, version, platform)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertEvidence("candidate", version+" "+platform+"/print claude-3 — candidate")
	interrupted := errors.New("fixture policy transport failed")
	service.SetPolicy = func(context.Context, string, bool) error { return interrupted }
	if _, err := runSetupTest(t, service, "setup", "y\nn\nn\n", false); !errors.Is(err, interrupted) {
		t.Fatalf("interrupted setup: %v", err)
	}
	assertEvidence("candidate", version+" "+platform+"/print claude-3 — candidate")
}

// Setup and doctor show these reasons to the owner. They state measured facts
// and carry no figure: a figure here could drift from the measurement that
// produced it, which no test in this repository can read.
func TestReminderReasonsStateFactsWithoutFigures(t *testing.T) {
	const codex = "The hook could not verify its launch version and mode. The tested post-tool matcher started on every Bash call."
	if got := installerFor("codex").reminderMeasurement().Reason; got != codex {
		t.Errorf("codex reason %q", got)
	}
	for adapter, i := range installers {
		if m := i.reminderMeasurement(); strings.ContainsAny(m.Reason, "0123456789") {
			t.Errorf("%s reason carries a figure: %q", adapter, m.Reason)
		}
	}
}
