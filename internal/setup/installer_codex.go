package setup

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// codexInstaller installs Codex hooks, which the owner trusts in /hooks and
// which carry the state root, and its command rules in a separate rules file.
type codexInstaller struct{ baseInstaller }

func (codexInstaller) defaultHome(home string) string { return filepath.Join(home, ".codex") }

func (codexInstaller) rulePath(hooks configTarget) string {
	return filepath.Join(filepath.Dir(hooks.Path), "rules", "crosshall.rules")
}

func (codexInstaller) skillPath(hooks configTarget) string { return vendorSkillPath(hooks) }

func (codexInstaller) trustHint() string {
	return "In Codex, run /hooks, review and trust the crosshall entries, then reopen Codex."
}

func (codexInstaller) sandboxNote() string {
	return "Codex allow rules run matching commands outside its sandbox."
}

func (codexInstaller) statePinned() bool { return true }

// recognizeShellCommand also accepts an earlier unpinned command.
func (codexInstaller) recognizeShellCommand(command, event string, _ hook.InstallSpec) bool {
	route, ok := parseCodexHookCommand(command, event)
	return ok && (route.root != "" || isCrosshallExecutable(route.executable))
}

func (codexInstaller) recordedExecutable(entry Entry, _ hook.InstallSpec) (string, error) {
	route, err := recordedCodexRoute(entry)
	return route.executable, err
}

func (codexInstaller) runsExecutable(command, old string, _ hook.InstallSpec) bool {
	_, command, _ = splitHookStatePin(strings.TrimSpace(command))
	return strings.HasPrefix(command, shellQuote(old))
}

// upgrade replaces earlier unpinned hooks with pinned ones in one edit.
func (codexInstaller) upgrade(target installTarget, record ownedFile, _ bool) ([]byte, [][]span, []Entry, []Entry, error) {
	base, migrate, err := codexInstallBase(target, record)
	if err != nil || !migrate {
		return base, nil, nil, nil, err
	}
	return base, record.Undo, record.Entries, nil, nil
}

// legacyHook is a recorded hook without the state-root pin.
func (codexInstaller) legacyHook(entry Entry) bool {
	route, err := recordedCodexRoute(entry)
	return err == nil && route.root == ""
}

func (codexInstaller) memberRules(_, executable string) ([]Entry, []byte, error) {
	text, err := codexMemberRules(executable)
	return nil, text, err
}

// skipRules leaves the rules to the owner when the executable is not named
// crosshall, or when another rule already names a crosshall host executable.
func (codexInstaller) skipRules(executable string, location configTarget, owned bool) (*retainedFile, error) {
	if filepath.Base(executable) != "crosshall" {
		return &retainedFile{"codex", location.Path, "skipped Codex command rules because the installed executable uses a different name"}, nil
	}
	return existingCodexHostRule(location.Path, owned)
}

func (codexInstaller) textRules() bool { return true }

func (codexInstaller) rulesChanged(path string) error {
	conflict, err := existingCodexHostRule(path, true)
	if err != nil {
		return err
	}
	if conflict != nil {
		return fmt.Errorf("Codex rules changed at %q after planning; rerun to review the new plan", conflict.Path)
	}
	return nil
}

func (codexInstaller) reminderMeasurement() reminderEvidence {
	return reminderEvidence{
		Version: "0.170.0", Platform: "darwin-arm64", Mode: "exec", ManifestRev: "codex-1", Level: "unavailable",
		Reason: "The hook could not verify its launch version and mode. The tested post-tool matcher started on every Bash call.",
	}
}

func (codexInstaller) hasMemberRules() bool { return true }
