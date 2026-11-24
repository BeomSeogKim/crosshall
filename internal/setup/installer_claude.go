package setup

import "path/filepath"

// claudeInstaller installs Claude Code hooks and command rules in one
// settings file.
type claudeInstaller struct{}

func (claudeInstaller) defaultHome(home string) string { return filepath.Join(home, ".claude") }

func (claudeInstaller) rulePath(hooks configTarget) string { return hooks.Path }

func (claudeInstaller) trustHint() string { return "" }

func (claudeInstaller) sandboxNote() string { return "" }
