package manifests

import "testing"

// The shipped manifests declare what the core used to infer from adapter
// names: only Claude Code names its process, only Codex runs a shared
// managed server, both check turn ends and recover a missed enrollment on a
// prompt, and Antigravity does none of these.
func TestShippedManifestsDeclareTheirCapabilities(t *testing.T) {
	want := map[string]Capabilities{
		"claude": {ProcessLaunch: true, TurnEnd: true, RecoverEnroll: true},
		"codex":  {TurnEnd: true, RecoverEnroll: true, SharedDaemon: true},
		"agy":    {},
	}
	for _, adapter := range Names() {
		got, ok := CapabilitiesOf(adapter)
		if !ok {
			t.Fatalf("%s: manifest not readable", adapter)
		}
		if got != want[adapter] {
			t.Errorf("%s: capabilities %+v, want %+v", adapter, got, want[adapter])
		}
	}
	if _, ok := CapabilitiesOf("none"); ok {
		t.Error("an adapter without a manifest has capabilities")
	}
}

// Display names are the ones setup status shows; an unknown adapter shows
// its ID.
func TestDisplayNameComesFromTheManifest(t *testing.T) {
	for adapter, want := range map[string]string{"claude": "Claude Code", "codex": "Codex", "agy": "Antigravity", "none": "none"} {
		if got := DisplayName(adapter); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", adapter, got, want)
		}
	}
}

// A stored profile resolves its exact revision. The prior Claude Code
// revisions keep what the core derived for them by adapter name: a process
// launch, a turn-end check and enrollment recovery.
func TestStoredRevisionsResolveExactly(t *testing.T) {
	claude := Capabilities{ProcessLaunch: true, TurnEnd: true, RecoverEnroll: true}
	for _, c := range []struct {
		adapter, revision string
		want              Capabilities
		ok                bool
	}{
		{"claude", "claude-1", claude, true},
		{"claude", "claude-2", claude, true},
		{"claude", "claude-3", claude, true},
		{"codex", "codex-1", Capabilities{TurnEnd: true, RecoverEnroll: true, SharedDaemon: true}, true},
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
}

// Every shipped manifest is readable and declares nothing this build
// cannot read.
func TestShippedManifestsAreValid(t *testing.T) {
	for _, adapter := range Names() {
		r, ok := records()[adapter]
		if !ok {
			t.Errorf("%s: manifest not readable", adapter)
		}
		for _, problem := range r.invalid {
			t.Errorf("%s: %s", adapter, problem)
		}
	}
}
