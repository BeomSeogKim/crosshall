package setup

import "path/filepath"

// agyInstaller installs Antigravity hooks under its config home and its
// command rules in the CLI's settings.
type agyInstaller struct{}

func (agyInstaller) defaultHome(home string) string { return filepath.Join(home, ".gemini", "config") }

func (agyInstaller) rulePath(hooks configTarget) string {
	return filepath.Join(hooks.Home, ".gemini", "antigravity-cli", "settings.json")
}

func (agyInstaller) trustHint() string { return "" }

func (agyInstaller) sandboxNote() string { return "" }
