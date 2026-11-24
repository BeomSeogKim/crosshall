package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func claudeInstallStatus(t *testing.T, service Service) InstallStatus {
	t.Helper()
	statuses := inspect(service.StateRoot, service.Executable)
	i := slices.IndexFunc(statuses, func(s InstallStatus) bool { return s.Adapter == "claude" })
	if i < 0 {
		t.Fatalf("no Claude status in %+v", statuses)
	}
	return statuses[i]
}

func claudeStatusText(t *testing.T, service Service) (AdapterStatus, string, string) {
	t.Helper()
	statuses, err := service.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var text, encoded bytes.Buffer
	if err := RenderStatus(&text, statuses, false); err != nil {
		t.Fatal(err)
	}
	if err := RenderStatus(&encoded, statuses, true); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(statuses, func(s AdapterStatus) bool { return s.Adapter == "claude" })
	if i < 0 {
		t.Fatalf("no Claude status in %+v", statuses)
	}
	return statuses[i], text.String(), encoded.String()
}

// Hooks left by an earlier revision are named with the revision that
// installed them, the one this binary installs and the command to run, in
// doctor's status and in setup status, until setup has installed the
// current ones.
func TestHooksOfAnEarlierRevisionAreNamedWithTheStep(t *testing.T) {
	_, service, path, _ := installClaude2(t)
	specs, err := selectedSpecs([]string{"claude"})
	if err != nil || len(specs) != 1 {
		t.Fatalf("Claude install spec: %v", err)
	}
	current := specs[0].Revision
	want := "The Claude Code hooks on disk were installed for claude-2; this crosshall installs " + current +
		". Run crosshall setup claude."

	status := claudeInstallStatus(t, service)
	if status.OutdatedHooks() != want || status.InstalledRev != "claude-2" {
		t.Fatalf("doctor status: sentence %q, installed revision %q; want %q", status.OutdatedHooks(), status.InstalledRev, want)
	}
	if status.InstallState != "partial" || status.Installed != status.Intended-1 {
		t.Fatalf("the state words changed: %+v", status)
	}
	row, text, encoded := claudeStatusText(t, service)
	if row.InstalledRev != "claude-2" || row.Install != "drift" || !strings.Contains(text, "\n  "+want+"\n") ||
		!strings.Contains(encoded, `"installed_rev":"claude-2"`) {
		t.Fatalf("setup status: %+v\n%s\n%s", row, text, encoded)
	}

	upgradeClaudeNotification(t, service, path)
	status = claudeInstallStatus(t, service)
	if status.OutdatedHooks() != "" || status.InstalledRev != current || status.InstallState != "installed" {
		t.Fatalf("after setup: sentence %q, %+v", status.OutdatedHooks(), status)
	}
	row, text, encoded = claudeStatusText(t, service)
	if row.InstalledRev != current || strings.Contains(text, "were installed for") ||
		!strings.Contains(encoded, `"installed_rev":"`+current+`"`) {
		t.Fatalf("setup status after setup: %+v\n%s\n%s", row, text, encoded)
	}
}

// A revision that changed no installed hook leaves nothing to rerun: every
// hook this binary installs is on disk.
func TestARevisionThatChangedNoHookIsNotOutdated(t *testing.T) {
	_, service, path, _ := installClaude2(t)
	upgradeClaudeNotification(t, service, path)
	state, err := openSetupState(service.StateRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	j, _, err := readJournalAt(state)
	if err != nil {
		t.Fatal(err)
	}
	b := j.Builds["claude"]
	b.Revision = "claude-2"
	j.Builds["claude"] = b
	if err := writeJournal(state, j); err != nil {
		t.Fatal(err)
	}

	status := claudeInstallStatus(t, service)
	if status.OutdatedHooks() != "" || status.InstalledRev != "claude-2" || status.InstallState != "installed" {
		t.Fatalf("sentence %q, %+v", status.OutdatedHooks(), status)
	}
	if _, text, _ := claudeStatusText(t, service); strings.Contains(text, "were installed for") {
		t.Fatalf("setup status: %s", text)
	}
}

// Without setup's record of the hooks on disk there is no revision to
// name: the same hooks, with the journal gone, get the old words only.
func TestHooksWithoutAJournalRecordAreNotCalledOutdated(t *testing.T) {
	_, service, _, _ := installClaude2(t)
	if err := os.Remove(filepath.Join(service.StateRoot, "install-journal.json")); err != nil {
		t.Fatal(err)
	}
	status := claudeInstallStatus(t, service)
	if status.OutdatedHooks() != "" || status.InstalledRev != "" || status.InstallState != "partial" {
		t.Fatalf("sentence %q, %+v", status.OutdatedHooks(), status)
	}
	row, text, encoded := claudeStatusText(t, service)
	if row.InstalledRev != "" || strings.Contains(text, "were installed for") || strings.Contains(encoded, "installed_rev") {
		t.Fatalf("setup status: %+v\n%s\n%s", row, text, encoded)
	}
}

// The sentence is about hooks on disk: once the owner has taken setup's
// entries out of the file, a journal that still names the old revision
// says nothing.
func TestRemovedHooksAreNotCalledOutdated(t *testing.T) {
	_, service, path, original := installClaude2(t)
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	status := claudeInstallStatus(t, service)
	if status.OutdatedHooks() != "" || status.InstalledRev != "" {
		t.Fatalf("sentence %q, %+v", status.OutdatedHooks(), status)
	}
	if _, text, encoded := claudeStatusText(t, service); strings.Contains(text, "were installed for") || strings.Contains(encoded, "installed_rev") {
		t.Fatalf("setup status: %s\n%s", text, encoded)
	}
}

// Current hooks the owner edited are drift, as before: the journal names
// the revision this binary installs, so there is no older one to report.
func TestEditedCurrentHooksAreNotCalledOutdated(t *testing.T) {
	_, service, path, _ := installClaude2(t)
	upgraded := upgradeClaudeNotification(t, service, path)
	edited := bytes.Replace(upgraded, []byte(`"matcher":"permission_prompt|idle_prompt"`), []byte(`"matcher":"permission_prompt"`), 1)
	if err := os.WriteFile(path, edited, 0640); err != nil {
		t.Fatal(err)
	}
	status := claudeInstallStatus(t, service)
	if status.OutdatedHooks() != "" || status.Installed != status.Intended-1 || status.InstallState != "drifted" {
		t.Fatalf("sentence %q, %+v", status.OutdatedHooks(), status)
	}
	if _, text, _ := claudeStatusText(t, service); strings.Contains(text, "were installed for") {
		t.Fatalf("setup status: %s", text)
	}
}
