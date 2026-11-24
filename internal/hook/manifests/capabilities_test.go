package manifests

import (
	"slices"
	"testing"
)

// Every revision the core can meet in a stored profile resolves to the
// capabilities the core derived for it before the catalog: by adapter name
// (process launch, turn end, recovery) and by the claude-1/claude-2
// exceptions of hook.HasPromptLine and hook.HasSessionEnd. Every Claude Code
// revision reports a shown permission dialog; Codex reports requests.
func TestRevisionCapabilitiesKeepTheirShippedMeaning(t *testing.T) {
	for _, c := range []struct {
		adapter, revision string
		want              Capabilities
		ok                bool
	}{
		{"claude", "claude-1", Capabilities{ProcessLaunch: true, TurnEnd: true, RecoverEnroll: true, RecoverEvent: "UserPromptSubmit", PromptLine: true, ApprovalDialog: true}, true},
		{"claude", "claude-2", Capabilities{ProcessLaunch: true, TurnEnd: true, RecoverEnroll: true, RecoverEvent: "UserPromptSubmit", PromptLine: true, SessionEnd: true, ApprovalDialog: true}, true},
		{"claude", "claude-3", Capabilities{ProcessLaunch: true, TurnEnd: true, RecoverEnroll: true, RecoverEvent: "UserPromptSubmit", PromptLine: true, SessionEnd: true, ApprovalDialog: true}, true},
		{"codex", "codex-1", Capabilities{TurnEnd: true, RecoverEnroll: true, RecoverEvent: "UserPromptSubmit", SharedDaemon: true, PromptLine: true, SessionEnd: true}, true},
		{"agy", "agy-1", Capabilities{}, true},
		{"claude", "claude-0", Capabilities{}, false},
		{"codex", "claude-3", Capabilities{}, false},
		{"none", "none-1", Capabilities{}, false},
	} {
		got, ok := CapabilitiesAt(c.adapter, c.revision)
		if got != c.want || ok != c.ok {
			t.Errorf("CapabilitiesAt(%q, %q) = %+v, %v; want %+v, %v", c.adapter, c.revision, got, ok, c.want, c.ok)
		}
	}
	for _, adapter := range Names() {
		shipped, _ := CapabilitiesOf(adapter)
		if at, _ := CapabilitiesAt(adapter, records()[adapter].revision); at != shipped {
			t.Errorf("%s: shipped revision %+v differs from CapabilitiesOf %+v", adapter, at, shipped)
		}
	}
	if _, ok := CapabilitiesOf("none"); ok {
		t.Error("an adapter without a manifest has capabilities")
	}
}

// Names for people keep the shipped wording: the display names setup
// status showed, the one alias setup accepted, and setup's adapter order.
func TestPresentationKeepsItsShippedWording(t *testing.T) {
	for adapter, want := range map[string]string{"claude": "Claude Code", "codex": "Codex", "agy": "Antigravity", "none": "none"} {
		if got := DisplayName(adapter); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", adapter, got, want)
		}
	}
	for name, want := range map[string]string{"antigravity": "agy", "agy": "agy", "claude": "claude", "other": "other", "--apply": "--apply"} {
		if got := Resolve(name); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", name, got, want)
		}
	}
	if got := SetupOrder(); !slices.Equal(got, []string{"claude", "codex", "agy"}) {
		t.Errorf("setup order %v", got)
	}
}

// The checks a build runs on its catalog: every manifest is readable,
// named after its file, of a supported schema, maps only declared events
// and capabilities, and no ID, alias or setup position is claimed twice.
func TestCatalogIsValid(t *testing.T) {
	claimed := make(map[string]string)
	claim := func(name, by string) {
		if other, taken := claimed[name]; taken {
			t.Errorf("%q is claimed by %s and %s", name, other, by)
		}
		claimed[name] = by
	}
	positions := make(map[int]string)
	for _, adapter := range Names() {
		r, ok := records()[adapter]
		if !ok {
			t.Errorf("%s: manifest not readable or not named after its file", adapter)
			continue
		}
		for _, problem := range r.invalid {
			t.Errorf("%s: %s", adapter, problem)
		}
		claim(adapter, adapter)
		for _, alias := range r.aliases {
			claim(alias, adapter+" alias")
		}
		if r.display == "" {
			t.Errorf("%s: no display name", adapter)
		}
		if other, taken := positions[r.setupOrder]; taken || r.setupOrder <= 0 {
			t.Errorf("%s: setup_order %d (taken by %q)", adapter, r.setupOrder, other)
		}
		positions[r.setupOrder] = adapter
	}
}
