package setup

import (
	"path/filepath"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// Every installable adapter has its bundle's installer, and each keeps the
// homes, rule files and owner review it had before the installers.
func TestEveryInstallableAdapterHasItsInstaller(t *testing.T) {
	home := "/Users/x"
	want := map[string]struct{ home, rules, trust, sandbox string }{
		"claude": {"/Users/x/.claude", "/Users/x/.claude/settings.json", "", ""},
		"codex": {"/Users/x/.codex", "/Users/x/.codex/rules/crosshall.rules",
			"Review and trust these hooks with Codex /hooks.", "Codex allow rules run matching commands outside its sandbox."},
		"agy": {"/Users/x/.gemini/config", "/Users/x/.gemini/antigravity-cli/settings.json", "", ""},
	}
	specs := hook.InstallSpecs()
	if len(specs) != len(want) {
		t.Fatalf("%d installable adapters, %d expected", len(specs), len(want))
	}
	for _, spec := range specs {
		i, ok := installerOf(spec)
		w, known := want[spec.Adapter]
		if !ok || !known {
			t.Fatalf("%s: installer %t, expected %t", spec.Adapter, ok, known)
		}
		hooks := configTarget{Home: home, Path: filepath.Join(i.defaultHome(home), spec.File)}
		if got := i.defaultHome(home); got != w.home {
			t.Errorf("%s home %q, want %q", spec.Adapter, got, w.home)
		}
		if got := i.rulePath(hooks); got != w.rules {
			t.Errorf("%s rules %q, want %q", spec.Adapter, got, w.rules)
		}
		if i.trustHint() != w.trust || i.sandboxNote() != w.sandbox {
			t.Errorf("%s trust %q sandbox %q", spec.Adapter, i.trustHint(), i.sandboxNote())
		}
	}
}

// A hooks-only adapter has no member command permissions: setup plans no
// rules for it instead of refusing. Every shipped adapter has them.
func TestAHooksOnlyAdapterGetsNoCommandRules(t *testing.T) {
	for _, spec := range hook.InstallSpecs() {
		if !installerFor(spec.Adapter).hasMemberRules() {
			t.Errorf("%s lost its member command rules", spec.Adapter)
		}
	}
	target := installTarget{Spec: hook.InstallSpec{Adapter: "hooksonly"}, Wanted: []Entry{{Event: "SessionStart"}}}
	if err := (Service{Executable: "/opt/crosshall"}).preflightCommandRules(&target, journal{}); err != nil ||
		target.CommandRules != nil || len(target.Wanted) != 1 {
		t.Fatalf("hooks-only adapter rules: %+v (%v)", target, err)
	}
}
