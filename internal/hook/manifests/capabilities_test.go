package manifests

import "testing"

// The shipped manifests declare what the core used to infer from adapter
// names: Claude Code and Codex check turn ends and recover a missed
// enrollment on a prompt; Antigravity does neither.
func TestShippedManifestsDeclareTheirCapabilities(t *testing.T) {
	want := map[string]Capabilities{
		"claude": {TurnEnd: true, RecoverEnroll: true},
		"codex":  {TurnEnd: true, RecoverEnroll: true},
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
