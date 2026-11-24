package setup

import "path/filepath"

// codexInstaller installs Codex hooks, which the owner trusts in /hooks,
// and its command rules in a separate rules file.
type codexInstaller struct{}

func (codexInstaller) defaultHome(home string) string { return filepath.Join(home, ".codex") }

func (codexInstaller) rulePath(hooks configTarget) string {
	return filepath.Join(filepath.Dir(hooks.Path), "rules", "crosshall.rules")
}

func (codexInstaller) trustHint() string { return "Review and trust these hooks with Codex /hooks." }

func (codexInstaller) sandboxNote() string {
	return "Codex allow rules run matching commands outside its sandbox."
}
