package setup

import (
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

func (codexInstaller) trustHint() string { return "Review and trust these hooks with Codex /hooks." }

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
