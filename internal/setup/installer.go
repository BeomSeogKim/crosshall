package setup

import "github.com/BeomSeogKim/crosshall/internal/hook"

// installer is an adapter bundle's part of setup: what differs between the
// vendors' configuration homes, files and owner review. Setup keeps the
// preview, consent, path checks, journal, atomic edits and uninstall
// ownership.
type installer interface {
	// defaultHome is the configuration home when the adapter's home
	// variable is unset.
	defaultHome(home string) string
	// rulePath places member command rules, given the hooks file target.
	rulePath(hooks configTarget) string
	// trustHint asks the owner to review installed hooks in the vendor, or
	// is empty when the vendor runs them without review.
	trustHint() string
	// sandboxNote warns that installed command rules run outside the
	// vendor's sandbox, or is empty.
	sandboxNote() string
}

func installerOf(spec hook.InstallSpec) (installer, bool) {
	i, ok := installers[spec.Adapter]
	return i, ok
}
