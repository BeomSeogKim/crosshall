package wire

import "testing"

// A prompt callback recovers a missed start only when its profile's exact
// revision declares recovery and the callback is that revision's recovering
// event. A revision the dispatcher does not know, as when newer hook files
// reach a dispatcher that is still running an older build, recovers nothing.
func TestRecoveryFollowsTheProfileRevision(t *testing.T) {
	base := HookEventArgs{EnrollRepoRoot: "/repo", WantOutput: true, Fact: HookPromptSubmitted, TurnRef: "t1",
		EventRef: "t1:UserPromptSubmit", HookAtMS: 1_000}
	for _, c := range []struct {
		name    string
		adapter string
		rev     string
		ref     string
		want    bool
	}{
		{"claude shipped", "claude", "claude-3", "", true},
		{"claude legacy", "claude", "claude-1", "", true},
		{"codex shipped", "codex", "codex-1", "", true},
		{"prompt entry reference", "claude", "claude-3", PromptEventRef("t1:UserPromptSubmit", 1_000), true},
		{"antigravity declares none", "agy", "agy-1", "", false},
		{"unknown revision", "claude", "claude-9", "", false},
		{"unknown adapter", "other", "other-1", "", false},
		{"another callback", "claude", "claude-3", "t1:Stop", false},
	} {
		args := base
		args.Profile = HookProfile{Adapter: c.adapter, ManifestRev: c.rev}
		if c.ref != "" {
			args.EventRef = c.ref
		}
		if got := args.CanRecoverEnrollment(); got != c.want {
			t.Errorf("%s: CanRecoverEnrollment = %v, want %v", c.name, got, c.want)
		}
	}
}
