package dispatch

import "testing"

// The routing warning names the adapter from the catalog; for Codex, the
// only adapter that declares a shared managed daemon, it reads as shipped.
func TestHookRoutingWarningKeepsTheCodexWording(t *testing.T) {
	const shared = "Codex hooks can run in a shared managed daemon and inherit its starting environment. "
	for routing, want := range map[string]string{
		"pinned":    shared + "The hook commands on disk pin the Crosshall state root selected at setup. Use that root for member commands in this Codex home.",
		"inherited": shared + "The hook commands on disk use that environment for state routing. Run crosshall setup codex to pin a root.",
		"mixed":     shared + "The hook commands on disk mix state roots or routing styles. Review them before running crosshall setup codex.",
		"uncertain": shared + "A recognized hook command on disk has uncertain state routing. Review it before running crosshall setup codex.",
		"":          "",
	} {
		if got := hookRoutingWarning("codex", routing); got != want {
			t.Errorf("%q: %q, want %q", routing, got, want)
		}
	}
}
