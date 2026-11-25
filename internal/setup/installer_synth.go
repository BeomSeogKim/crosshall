//go:build synthadapter

package setup

import "path/filepath"

// synthInstaller installs the synth fixture's hooks with the shared
// behavior; its home is ~/.synth.
type synthInstaller struct{ baseInstaller }

func (synthInstaller) defaultHome(home string) string { return filepath.Join(home, ".synth") }

func (synthInstaller) rulePath(hooks configTarget) string { return hooks.Path }

func init() { installers["synth"] = synthInstaller{} }
