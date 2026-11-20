package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func evidenceFixture() AdapterEvidence {
	return AdapterEvidence{Vendor: "claude", VendorVersion: "2.1.327", Platform: "darwin-arm64", Mode: "print",
		ManifestRev: "claude-1", RunAtMS: 1_800_000_000_000, ResultRef: "sha256:00",
		Results: []EvidenceResult{
			{Cap: "enroll", Level: "tested", Check: "hint in the first request"},
			{Cap: "boundary", Level: "tested", Check: "pending line once at a prompt"},
			{Cap: "observe", Level: "tested", Check: "prompt and stop facts"},
			{Cap: "reminder", Level: "candidate", Check: "not run"},
		}}
}

func TestOwnerRecordsConformanceEvidence(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	f.hook(s, start("pid-1", "startup", f.now.UnixMilli()))
	cmd := evidenceFixture()
	cmd.Platform, cmd.Mode = testProfile.Platform, testProfile.Mode // the member's launch profile
	cmd.VendorVersion, cmd.ManifestRev = testProfile.VendorVersion, testProfile.ManifestRev
	wantOK(t, f.owner(cmd, "evidence-1"))
	if n := f.count(`SELECT COUNT(*) FROM capability_evidence WHERE vendor = 'claude' AND level = 'tested'`); n != 3 {
		t.Fatalf("%d tested rows", n)
	}
	// The live member's projected capability now carries the tested level.
	if n := f.count(`SELECT COUNT(*) FROM capability WHERE principal_id = ? AND cap = 'enroll' AND level = 'tested'`, f.principalOf(s)); n != 1 {
		t.Fatal("the member's capability did not take the recorded level")
	}
	// Re-importing a newer run replaces its rows; the same key replays.
	wantOK(t, f.owner(cmd, "evidence-1"))
	if n := f.count(`SELECT COUNT(*) FROM capability_evidence`); n != 4 {
		t.Fatalf("%d rows after a replay", n)
	}
}

func TestEvidenceIsRefusedFromMembersAndWhenMalformed(t *testing.T) {
	f, writer := adapterChannel(t)
	wantCode(t, f.run(writer, evidenceFixture(), "member"), core.ExitRefused, core.CodeOwnerOnly)
	for name, mutate := range map[string]func(*AdapterEvidence){
		"vendor":  func(e *AdapterEvidence) { e.Vendor = "unknown" },
		"cap":     func(e *AdapterEvidence) { e.Results[0].Cap = "wake" },
		"level":   func(e *AdapterEvidence) { e.Results[0].Level = "maybe" },
		"version": func(e *AdapterEvidence) { e.VendorVersion = "" },
		"empty":   func(e *AdapterEvidence) { e.Results = nil },
		"ref":     func(e *AdapterEvidence) { e.ResultRef = "" },
	} {
		cmd := evidenceFixture()
		mutate(&cmd)
		if r := f.owner(cmd, "bad-"+name); r.Status != core.ExitUsage {
			t.Errorf("%s: status %d", name, r.Status)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM capability_evidence`); n != 0 {
		t.Fatalf("refusals wrote %d rows", n)
	}
}

func TestEvidenceCannotQualifyAnUnverifiedLaunchOrRollBack(t *testing.T) {
	f, _ := adapterChannel(t)
	for name, mutate := range map[string]func(*AdapterEvidence){
		"unverified version": func(e *AdapterEvidence) { e.VendorVersion = "unverified" },
		"unverified mode":    func(e *AdapterEvidence) { e.Mode = "unverified" },
		"duplicate cap":      func(e *AdapterEvidence) { e.Results = append(e.Results, e.Results[0]) },
		"tested, no check":   func(e *AdapterEvidence) { e.Results[0].Check = "" },
	} {
		cmd := evidenceFixture()
		mutate(&cmd)
		if r := f.owner(cmd, "bad-"+name); r.Status != core.ExitUsage {
			t.Errorf("%s: status %d", name, r.Status)
		}
	}
	newer := evidenceFixture()
	newer.RunAtMS, newer.ResultRef = 2_000, "sha256:newer"
	wantOK(t, f.owner(newer, "newer"))
	older := evidenceFixture()
	older.RunAtMS, older.ResultRef = 1_000, "sha256:older"
	older.Results[0].Level = "unavailable"
	wantOK(t, f.owner(older, "older"))
	if n := f.count(`SELECT COUNT(*) FROM capability_evidence WHERE result_ref = 'sha256:older'`); n != 0 {
		t.Fatalf("an older run overwrote %d newer rows", n)
	}
	before := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'adapter_evidence'`)
	wantOK(t, f.owner(newer, "newer-again")) // the same file under a new key
	if after := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'adapter_evidence'`); after != before {
		t.Fatal("an identical reimport changed the store")
	}
}
