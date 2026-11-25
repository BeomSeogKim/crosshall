//go:build synthadapter

package ident

// The synth fixture's identity, as its manifest declares it;
// TestIdentityIndexCoversEveryManifest checks the two agree.
func init() {
	identityIndex = append(identityIndex, identityRow{adapter: "synth", sessionEnv: "SYNTH_SESSION_ID", evidence: "observed",
		verifiedOn: []string{"1.0.0"}, markers: []string{"SYNTH_AGENT"}, cliName: "Synth Fixture",
		version: versionSource{env: "SYNTH_VERSION"}})
}
