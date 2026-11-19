package setup

import (
	"os"
	"path/filepath"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Inspect reports user-level configuration without launching either vendor.
func Inspect(stateRoot string) []InstallStatus {
	j, _, journalErr := readJournal(stateRoot)
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	var statuses []InstallStatus
	for _, spec := range hook.InstallSpecs() {
		status := InstallStatus{
			Adapter: spec.Adapter, Executable: unknown, Version: unknown, InstallState: unknown,
			Trust: unknown, Intended: len(spec.Events),
		}
		if b, err := currentBuild(spec); err == nil {
			status.Executable = b.Executable
			status.platform, status.revision = b.Platform, b.Revision
			cached := j.Builds[spec.Adapter]
			b.Version = cached.Version
			if cached == b && cached.Version != "" {
				status.Version = cached.Version
			}
		}
		status.SetReminderEvidence(nil)
		if err == nil && journalErr == nil && j.Pending == nil {
			inspectInstall(&status, spec, executable, j.Files[spec.Adapter])
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// SetReminderEvidence adds applicable imported results to install diagnostics.
func (s *InstallStatus) SetReminderEvidence(evidence []store.DoctorEvidence) {
	s.Reminder = releaseReminderStatus(s.Adapter, s.Version, s.platform, s.revision, evidence)
}

func inspectInstall(status *InstallStatus, spec hook.InstallSpec, executable string, record ownedFile) {
	home, path, err := configPath(spec)
	if err != nil || verifyHome(home) != nil {
		return
	}
	current, err := readConfig(path)
	if err != nil || (current.Exists && len(current.Bytes) == 0) {
		return
	}
	expected, err := hookEntries(spec, executable)
	if err != nil {
		return
	}
	counts, err := entryMatches(current.Bytes, expected)
	if err != nil {
		return
	}
	for _, count := range counts {
		if count > 0 {
			status.Installed++
		}
		if count > 1 {
			status.Drifted++
		}
	}
	if record.Path != "" {
		if path != record.Path || current.DirIdentity != record.Latest.Dir {
			status.Drifted += len(record.Entries)
		} else {
			_, kept, err := removeEntries(current.Bytes, record.Entries)
			if err != nil {
				return
			}
			status.Drifted += len(kept)
		}
	}
	switch {
	case status.Drifted > 0:
		status.InstallState = "drifted"
	case status.Installed == status.Intended:
		status.InstallState = "installed"
	case status.Installed > 0:
		status.InstallState = "partial"
	default:
		status.InstallState = "not_installed"
	}
	if spec.Adapter == "codex" && status.Installed > 0 {
		status.Trust = "owner_review"
	}
}
