package domain

import (
	"slices"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// A hook of a newer build names a manifest revision this dispatcher does not
// know. Its enrollment recovery is refused as before, with the same exit, but
// the refusal says why and the dispatcher records the revision for owner
// doctor. Known and legacy revisions, other refusals and warm member
// callbacks record nothing.
func TestAnUnknownRevisionRefusesRecoveryWithItsOwnMessage(t *testing.T) {
	f, _ := adapterChannel(t)
	var noted [][2]string
	f.svc.UnknownRevision = func(adapter, revision string) { noted = append(noted, [2]string{adapter, revision}) }
	at := f.now.UnixMilli()

	bindings := f.count(`SELECT COUNT(*) FROM binding`)
	newer := f.newSession("claude")
	cmd := recoveryPrompt(SessionLaunch, "newer", at+10)
	cmd.Profile.ManifestRev = "claude-9"
	r := f.run(newer, cmd, "")
	if r.Status != core.ExitUsage || decode[Refusal](t, r).Message != UnknownRevisionMessage("claude-9") {
		t.Fatalf("mixed-binary recovery: %+v", r)
	}
	if !slices.Equal(noted, [][2]string{{"claude", "claude-9"}}) {
		t.Fatalf("noted %v", noted)
	}
	if f.count(`SELECT COUNT(*) FROM binding`) != bindings {
		t.Fatal("the refused prompt joined a member")
	}

	// An unknown adapter keeps the generic refusal and is not recorded.
	other := recoveryPrompt(SessionLaunch, "other", at+20)
	other.Profile.Adapter, other.Profile.ManifestRev = "zed", "zed-1"
	if r := f.run(f.newSession("claude"), other, ""); r.Status != core.ExitUsage || decode[Refusal](t, r).Message != "invalid hook.event arguments" {
		t.Fatalf("unknown adapter: %+v", r)
	}
	// A known revision whose recovery is refused for another reason (no
	// output wanted) keeps the generic refusal.
	quiet := recoveryPrompt(SessionLaunch, "quiet", at+30)
	quiet.WantOutput = false
	if r := f.run(f.newSession("claude"), quiet, ""); r.Status != core.ExitUsage || decode[Refusal](t, r).Message != "invalid hook.event arguments" {
		t.Fatalf("known revision, no output: %+v", r)
	}
	// The legacy revision still recovers.
	legacy := recoveryPrompt(SessionLaunch, "legacy", at+40)
	legacy.Profile.ManifestRev = "claude-1"
	if got := f.hook(f.newSession("claude"), legacy); got.Enrollment == "" {
		t.Fatalf("legacy revision did not recover: %+v", got)
	}
	// A warm member callback with an unknown revision is not a recovery.
	member := f.newSession("claude")
	wantOK(t, f.join(member, Join{Channel: "checkout", Alias: "member"}, ""))
	warm := prompt(SessionLaunch, "warm", at+50)
	warm.Profile.ManifestRev = "claude-9"
	wantOK(t, f.run(member, warm, ""))
	if len(noted) != 1 {
		t.Fatalf("other callbacks were recorded: %v", noted)
	}
}
