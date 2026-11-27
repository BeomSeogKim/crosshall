package setup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/setuptest"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type readmeTerminal struct {
	bytes.Buffer
	beforeInput string
}

func (t *readmeTerminal) Read([]byte) (int, error) {
	t.beforeInput = t.String()
	return 0, io.EOF
}

func hostInstallEvidence(vendor, revision string) []store.BuiltinEvidence {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	var evidence []store.BuiltinEvidence
	for _, row := range domain.BuiltinCapabilityEvidence {
		if row.Vendor == vendor && row.Platform == platform && row.ManifestRev == revision {
			evidence = append(evidence, row)
		}
	}
	return evidence
}

func hasInstallCapability(evidence []store.BuiltinEvidence, capability, mode string) bool {
	return slices.ContainsFunc(evidence, func(row store.BuiltinEvidence) bool {
		return row.Capability == capability && row.Mode == mode
	})
}

func expectedInstallNotice(evidence []store.BuiltinEvidence, mode, version, notice string) string {
	if !hasInstallCapability(evidence, "publish", mode) {
		return ""
	}
	if slices.ContainsFunc(evidence, func(row store.BuiltinEvidence) bool {
		return row.Capability == "publish" && row.Mode == mode && row.Version == version
	}) {
		return notice
	}
	return notice + newVersionNotice
}

// trustedTurnEnd is the caveat a turn-end line carries when the build has no
// tested evidence of its own and version's evidence covers it.
func trustedTurnEnd(version string) string {
	return " This version is new to crosshall: the check is trusted from Claude Code " + version + " evidence."
}

// expectedTurnEndTrust is the caveat for version from shipped evidence: the
// latest tested build of the interactive mode, else of print mode, unless
// version has evidence of its own.
func expectedTurnEndTrust(evidence []store.BuiltinEvidence, version string) string {
	for _, mode := range []string{"interactive", "print"} {
		var latest store.BuiltinEvidence
		for _, row := range evidence {
			if row.Capability != "turn_end" || row.Mode != mode {
				continue
			}
			if row.Version == version {
				latest = store.BuiltinEvidence{}
				break
			}
			if row.CheckedAtMS > latest.CheckedAtMS {
				latest = row
			}
		}
		if latest.Version != "" {
			return trustedTurnEnd(latest.Version)
		}
	}
	return ""
}

func TestInstallerPromptDisclosureMatchesTheListedAgent(t *testing.T) {
	for _, tc := range []struct {
		adapter string
		prompt  bool
	}{{"codex", true}, {"agy", true}} {
		t.Run(tc.adapter, func(t *testing.T) {
			f := setuptest.New(t, tc.adapter)
			out, err := realRun(t, scopedService(f), f, Request{Offer: true, Terminal: true}, "\n")
			pickup := "its next crosshall read."
			if tc.prompt {
				pickup = "its next prompt or " + pickup
			}
			want := "- Notices and the turn-end check are not available for these builds. A session sees new work at " + pickup
			if tc.adapter == "codex" {
				if notice := expectedInstallNotice(hostInstallEvidence("codex", "codex-1"), "daemon", "0.170.0", codexNoticeOn); notice != "" {
					want = notice
				}
			}
			if tc.adapter == "agy" && hasInstallCapability(hostInstallEvidence("agy", "agy-2"), "turn_end", "interactive") {
				want = "- Turn-end check (Antigravity, interactive sessions). When a turn ends while a request waits that is unread, unanswered and not yet announced to the session, the turn continues once. Each continuation costs one model request. Off: crosshall owner adapter set agy --turn-end off"
			}
			if err != nil || strings.Contains(out, "- Prompt line (Claude Code, Codex CLI, Antigravity).") != tc.prompt ||
				!strings.Contains(out, want) {
				t.Fatalf("offer: %v\n%s", err, out)
			}
		})
	}
}

func TestInstallerDisclosesPossibleCostsWhenTheClaudeVersionProbeFails(t *testing.T) {
	for _, tc := range []struct {
		name, notice string
		hallOff      bool
	}{
		{"default", "- Notices (Claude Code, interactive sessions). crosshall could not read this Claude Code's version, so it cannot say here whether notices are available for it. If they are, the hall starts a turn in an idle session when work arrives, and each notice costs that session one model turn. Off: crosshall owner publish hall off", false},
		{"hall off", "- Notices (Claude Code, interactive sessions). Off by your setting; installing does not change it. On: crosshall owner publish hall on", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _ := realSetup(t)
			if err := os.WriteFile(filepath.Join(f.Bin, "claude"), []byte("#!/bin/sh\nexit 19\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if tc.hallOff {
				if err := os.Mkdir(f.State, 0700); err != nil {
					t.Fatal(err)
				}
				db, err := store.Open(t.Context(), f.State)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if err := db.Update(t.Context(), func(tx store.Tx) error {
					_, err := tx.ExecContext(t.Context(), `INSERT INTO hall_publish VALUES (1, 0, 1)`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			out, err := realRun(t, s, f, Request{Offer: true, Terminal: true}, "\n")
			turnEnd := "- Turn-end check (Claude Code). crosshall could not read this Claude Code's version, so it cannot say here whether the check is available for it. If it is, a turn that ends while a request waits that is unread, unanswered and not yet announced to the session continues once, and each continuation costs one model request. Off: crosshall owner adapter set claude --turn-end off"
			if err != nil || !strings.Contains(out, tc.notice+"\n"+turnEnd+"\n") ||
				!strings.Contains(out, "A no skips future installer offers") ||
				strings.Contains(out, "not available for these builds") {
				t.Fatalf("offer: %v\n%s", err, out)
			}
		})
	}
}

// The consent matrix supplies known builds. The probe failure and first-install
// tests exercise vendor discovery through the real setup operations.
func consentFixturePlans(t *testing.T, s Service) *fakeOps {
	t.Helper()
	ops := newFakeOps()
	for _, plan := range knownBuildPlans(t, s, "claude", "codex") {
		ops.plans[plan.Adapter] = plan
	}
	return ops
}

// knownBuildPlans plans an install as PlanInstall does, with the fixture's
// versions in place of the one-second version probe.
func knownBuildPlans(t *testing.T, s Service, adapters ...string) []VendorPlan {
	t.Helper()
	specs, err := selectedSpecs(adapters)
	if err != nil {
		t.Fatal(err)
	}
	j := emptyJournal()
	versions := map[string]string{"claude": "2.1.327", "codex": "0.170.0"}
	var targets []installTarget
	for _, spec := range specs {
		target, err := s.preflightInstall(spec, j)
		if err != nil {
			t.Fatal(err)
		}
		target.Build.Version = versions[spec.Adapter]
		targets = append(targets, target)
	}
	s.attachEvidence(t.Context(), targets)
	var plans []VendorPlan
	for i := range targets {
		single := targets[i : i+1]
		op, err := makeInstall(j, single)
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, newVendorPlan(op, single, j, "", false))
	}
	return plans
}

const claudeNoticeOn = "- Notices (Claude Code, interactive sessions). When work arrives for an idle session, the hall starts a turn in it. Each notice costs that session one model turn. Off: crosshall owner publish hall off"
const codexNoticeOn = "- Notices (Codex, managed daemon sessions). When work arrives for an idle session, the hall starts a turn in it. Each notice costs that session one model turn. Off: crosshall owner publish hall off"
const newVersionNotice = " This version is new to crosshall: the first three notices are checked, and a bad one stops notices until you resume them."

func TestInstallerConsentDisclosesOnlyTheApplicableEffects(t *testing.T) {
	const noticeOn = claudeNoticeOn
	const turnEndBody = "When a turn ends while a request waits that is unread, unanswered and not yet announced to the session, the turn continues once. Each continuation costs one model request. Off: crosshall owner adapter set claude --turn-end off"
	const turnEndOn = "- Turn-end check (Claude Code, interactive sessions and print mode). " + turnEndBody
	const noticeUnavailable = "- Notices (Claude Code, interactive sessions). Not available for this build. A session sees new work at its next prompt or its next crosshall read."
	const turnEndUnavailable = "- Turn-end check (Claude Code). Not available for this build."
	const unavailable = "- Notices and the turn-end check are not available for these builds. A session sees new work at its next prompt or its next crosshall read."
	for _, tc := range []struct {
		name, version, publishLevel, turnEndLevel, printLevel, settings, notice, turnEnd, codexNotice string
	}{
		{name: "exact", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn, turnEnd: turnEndOn},
		{name: "print only", version: "2.1.327", publishLevel: "tested", turnEndLevel: "unavailable", printLevel: "tested",
			notice: noticeOn, turnEnd: "- Turn-end check (Claude Code, print mode). " + turnEndBody},
		{name: "interactive only", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", printLevel: "unavailable",
			notice: noticeOn, turnEnd: "- Turn-end check (Claude Code, interactive sessions). " + turnEndBody},
		{name: "borrowed", version: "2.1.326", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn + newVersionNotice,
			turnEnd: turnEndOn},
		{name: "borrowed per mode", version: "2.1.326", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn + newVersionNotice,
			turnEnd: turnEndOn + " This version is new to crosshall: the check is trusted from Claude Code 2.1.324 evidence for interactive sessions and from Claude Code 2.1.323 evidence for print mode.",
			// Later than any shipped evidence, so each mode names its own build.
			settings: `INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms) VALUES
				('claude', '2.1.324', ?1, 'interactive', 'claude-3', 'turn_end', 'tested', 'fixture', 9000000000000),
				('claude', '2.1.323', ?1, 'print', 'claude-3', 'turn_end', 'tested', 'fixture', 9000000000000)`},
		{name: "seen", version: "2.1.326", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn,
			turnEnd: turnEndOn,
			settings: `INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, confirmed)
				VALUES ('claude', '2.1.327', ?1, 'interactive', 'claude-3', 'publish', 3)`},
		{name: "owner hall off", version: "2.1.327", publishLevel: "tested", turnEndLevel: "candidate",
			settings: `INSERT INTO hall_publish VALUES (1, 0, 1); INSERT INTO hall_auto_join VALUES (1, 0, 1)`,
			notice:   "- Notices (Claude Code, interactive sessions). Off by your setting; installing does not change it. On: crosshall owner publish hall on",
			turnEnd:  "- Turn-end check (Claude Code). Not available for this build."},
		{name: "owner adapter off", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", turnEnd: turnEndOn,
			settings: `INSERT INTO hall_publish VALUES (1, 0, 1);
				INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, publish) VALUES ('claude', 0, 0, 1, 1, 0)`,
			notice: "- Notices (Claude Code, interactive sessions). Off by your setting for Claude Code; installing does not change it. On: crosshall owner adapter set claude --publish on"},
		{name: "owner adapter on", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", turnEnd: turnEndOn,
			settings: `INSERT INTO hall_publish VALUES (1, 0, 1);
				INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, publish) VALUES ('claude', 0, 0, 1, 1, 1)`,
			notice: strings.Replace(noticeOn, "crosshall owner publish hall off", "crosshall owner adapter set claude --publish off", 1)},
		{name: "codex adapter off", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn, turnEnd: turnEndOn,
			settings:    `INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, publish) VALUES ('codex', 1, 0, 1, 1, 0)`,
			codexNotice: "- Notices (Codex, managed daemon sessions). Off by your setting for Codex; installing does not change it. On: crosshall owner adapter set codex --publish on"},
		{name: "stopped", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", turnEnd: turnEndOn,
			settings: `INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, stopped_at_ms, stop_reason, stop_ref)
				VALUES ('claude', '2.1.327', ?1, 'interactive', 'claude-3', 'publish', 1, 'absorbed', 41)`,
			notice: "- Notices (Claude Code, interactive sessions). Stopped for this version after a notice went wrong. crosshall owner who --all shows how to resume."},
		{name: "turn-end switch off", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn,
			settings: `INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, turn_end) VALUES ('claude', 1, 0, 1, 1, 0)`,
			turnEnd:  "- Turn-end check (Claude Code). Off by your setting; installing does not change it. On: crosshall owner adapter set claude --turn-end on"},
		{name: "turn-end switch on", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn, turnEnd: turnEndOn,
			settings: `INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, turn_end) VALUES ('claude', 1, 0, 1, 1, 1)`},
		{name: "turn-end switch off, uncovered", version: "2.1.327", publishLevel: "tested", turnEndLevel: "candidate", notice: noticeOn,
			settings: `INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, turn_end) VALUES ('claude', 1, 0, 1, 1, 0)`,
			turnEnd:  "- Turn-end check (Claude Code). Not available for this build."},
		{name: "exact candidate", version: "2.1.327", publishLevel: "candidate", turnEndLevel: "candidate", notice: unavailable},
		{name: "publish unavailable", version: "2.1.327", publishLevel: "candidate", turnEndLevel: "tested", turnEnd: turnEndOn,
			notice: noticeUnavailable},
		{name: "cold"},
		{name: "unreadable", notice: "- Notices and the turn-end check. crosshall could not read its settings, so it cannot say here whether they are on for these builds. After installing, crosshall owner doctor shows it."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _ := realSetup(t)
			ctx := t.Context()
			platform := runtime.GOOS + "-" + runtime.GOARCH
			notice, turnEnd, codexNotice := tc.notice, tc.turnEnd, tc.codexNotice
			ledger := filepath.Join(f.State, "store.sqlite3")
			if tc.name == "borrowed" || tc.name == "seen" {
				// 2.1.327 has no turn-end evidence of its own: the line names
				// the latest tested build, the fixture's or a shipped one.
				fixture := store.BuiltinEvidence{Vendor: "claude", Version: "2.1.326", Platform: platform,
					Mode: "interactive", ManifestRev: "claude-3", Capability: "turn_end", CheckedAtMS: 1}
				turnEnd += expectedTurnEndTrust(append(hostInstallEvidence("claude", "claude-3"), fixture), "2.1.327")
			}
			if tc.name == "cold" {
				claude := hostInstallEvidence("claude", "claude-3")
				interactiveTurnEnd := hasInstallCapability(claude, "turn_end", "interactive")
				printTurnEnd := hasInstallCapability(claude, "turn_end", "print")
				switch {
				case interactiveTurnEnd && printTurnEnd:
					turnEnd = turnEndOn
				case interactiveTurnEnd:
					turnEnd = "- Turn-end check (Claude Code, interactive sessions). " + turnEndBody
				case printTurnEnd:
					turnEnd = "- Turn-end check (Claude Code, print mode). " + turnEndBody
				}
				if turnEnd != "" {
					turnEnd += expectedTurnEndTrust(claude, "2.1.327")
				}
				notice = expectedInstallNotice(claude, "interactive", "2.1.327", noticeOn)
				if notice != "" {
					if turnEnd == "" {
						turnEnd = turnEndUnavailable
					}
				} else if turnEnd != "" {
					notice = noticeUnavailable
				}
				codexNotice = expectedInstallNotice(hostInstallEvidence("codex", "codex-1"), "daemon", "0.170.0", codexNoticeOn)
				if notice == "" && codexNotice == "" {
					notice = unavailable
				}
			} else if tc.name == "unreadable" {
				if err := os.Mkdir(f.State, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ledger, []byte("not a database"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(f.State, 0700); err != nil {
					t.Fatal(err)
				}
				db, err := store.Open(ctx, f.State)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				printLevel := tc.printLevel
				if printLevel == "" {
					printLevel = tc.turnEndLevel
				}
				codexLevel := "candidate"
				if codexNotice != "" {
					codexLevel = "tested"
				}
				err = db.Update(ctx, func(tx store.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
						(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms) VALUES
						('claude', ?2, ?1, 'interactive', 'claude-3', 'publish', ?3, 'fixture', 1),
						('claude', ?2, ?1, 'interactive', 'claude-3', 'turn_end', ?4, 'fixture', 1),
						('claude', ?2, ?1, 'print', 'claude-3', 'turn_end', ?5, 'fixture', 1),
						('claude', '2.1.325', ?1, 'interactive', 'claude-3', 'publish', 'tested', 'older', 1),
						('codex', '0.170.0', ?1, 'daemon', 'codex-1', 'publish', ?6, 'fixture', 1)`,
						platform, tc.version, tc.publishLevel, tc.turnEndLevel, printLevel, codexLevel)
					if err == nil && tc.settings != "" {
						_, err = tx.ExecContext(ctx, tc.settings, platform)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			offer := tc.name != "owner adapter off" && tc.name != "owner adapter on" && tc.name != "unreadable" &&
				!strings.HasPrefix(tc.name, "turn-end switch")
			var terminal readmeTerminal
			req := Request{Action: "setup", Offer: offer, Terminal: true, Input: strings.NewReader("\n"), Output: &terminal}
			if tc.name == "exact" {
				req.PathConsent = func(bool) error { return nil }
				req.Input = &terminal
			}
			f.CheckTargets(t)
			if err := s.prompts(ctx, req, consentFixturePlans(t, s)); err != nil {
				t.Fatal(err)
			}
			out := terminal.String()
			const heading = "Saying yes turns these on for the listed agents:\n"
			_, disclosure, found := strings.Cut(out, heading)
			if tc.name == "exact" {
				_, question, asked := strings.Cut(terminal.beforeInput, heading)
				got := strings.ReplaceAll(strings.TrimRight(heading+question, " \t\r\n"), f.Home, "/home/you") + "\n"
				want := setuptest.ReadmeBlock(t, "install-question")
				if !asked || got != want {
					t.Fatalf("README install question differs from the terminal offer\ngot:\n%s\nwant:\n%s", got, want)
				}
			}
			end := "Install the listed hooks and command rules?"
			if offer {
				end = "A no skips future installer offers"
			}
			block, _, ends := strings.Cut(disclosure, end)
			joining := "- Joining. A new session joins its repository's channel when it starts. A Git repository with no channel gets one, named after its directory. Off for repositories without a channel: crosshall owner channel auto off. A repository that has a channel keeps joining its sessions while the agent's hooks are on."
			if tc.name == "owner hall off" {
				joining = "- Joining. A new session joins its repository's channel when it starts, in repositories that have one. You turned automatic joining off, so a repository without a channel gets none. On: crosshall owner channel auto on"
			} else if tc.name == "unreadable" {
				joining = "- Joining. A new session joins its repository's channel when it starts, in repositories that have one. Whether a repository without a channel gets one depends on your setting (crosshall owner channel auto on|off)."
			}
			want := joining + "\n- Prompt line (Claude Code, Codex CLI, Antigravity). A joined session sees one line with the numbers of its unread messages at its next prompt, and crosshall read picks them up. The line itself costs no model request.\n"
			for _, line := range []string{notice, turnEnd, codexNotice} {
				if line != "" {
					want += line + "\n"
				}
			}
			want += "- Skill. Setup installs a crosshall skill that teaches the agent the hall's commands. It adds only its short description to the agent's context, and the agent loads the rest when a request needs it.\n"
			if !found || !ends || block != want || strings.Count(out, "[y/N]") != 1 {
				t.Fatalf("offer:\n%s\nwant disclosure:\n%s", out, want)
			}
			if tc.name == "cold" {
				if _, err := os.Lstat(ledger); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("preview ledger stat: %v, want no store", err)
				}
			}
		})
	}
}

// #207: before the first dispatcher start there is no store, and the plan
// lists the built-in evidence the install question already answers from,
// not "unknown", for the build that evidence names.
func TestTheFirstInstallListsTheBuiltInEvidence(t *testing.T) {
	s, f, _ := realSetup(t)
	stub := "#!/bin/sh\n[ \"$1\" = --version ] || exit 2\nprintf '%s\\n' '2.1.331 (Claude Code)'\n"
	if err := os.WriteFile(filepath.Join(f.Bin, "claude"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	stub = "#!/bin/sh\n[ \"$1\" = --version ] || exit 2\nprintf '%s\\n' 'codex-cli 0.172.0'\n"
	if err := os.WriteFile(filepath.Join(f.Bin, "codex"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := realRun(t, s, f, Request{Offer: true, Terminal: true}, "\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.State, "store.sqlite3")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the preview created a store: %v", err)
	}
	if strings.Contains(out, "could not read this Claude Code's version") {
		t.Fatalf("the question treats a build whose version was read as unknown:\n%s", out)
	}
	for _, build := range []struct {
		vendor, revision, version, mode, notice string
	}{
		{"claude", "claude-3", "2.1.331", "interactive", claudeNoticeOn},
		{"codex", "codex-1", "0.172.0", "daemon", codexNoticeOn},
	} {
		evidence := hostInstallEvidence(build.vendor, build.revision)
		var want, listed []string
		for _, row := range evidence {
			if row.Version == build.version {
				want = append(want, fmt.Sprintf("  %s %s/%s tested", row.Capability, row.Platform, row.Mode))
			}
		}
		_, vendorBlock, found := strings.Cut(out, "\n"+build.vendor+" "+build.version+" ")
		_, details, _ := strings.Cut(vendorBlock, "\n")
		unknown := false
		for _, line := range strings.Split(details, "\n") {
			if !strings.HasPrefix(line, "  ") {
				break
			}
			if fields := strings.Fields(line); len(fields) == 3 && strings.Contains(fields[1], "/") {
				listed = append(listed, line)
			}
			unknown = unknown || line == "  Capability evidence for this build: unknown"
		}
		slices.Sort(want)
		slices.Sort(listed)
		wantNotice := expectedInstallNotice(evidence, build.mode, build.version, build.notice)
		noticeMatches := !strings.Contains(out, build.notice)
		if wantNotice != "" {
			noticeMatches = strings.Contains(out, wantNotice+"\n")
		}
		if !found || !slices.Equal(listed, want) || unknown != (len(want) == 0) || !noticeMatches {
			t.Fatalf("%s listing %q, want %q, or question disagrees:\n%s", build.vendor, listed, want, out)
		}
	}
}

// Antigravity runs no later Stop hook once one continues the turn (#328):
// the offer says so when another group in the hooks file has Stop hooks.
func TestTheOfferNamesAnotherGroupsStopHooks(t *testing.T) {
	const line = "- Other Stop hooks (Antigravity). A hook group in the same hooks file whose name sorts after crosshall has Stop hooks."
	for _, tc := range []struct {
		name, hooks string
		want        bool
	}{
		{"no other group", "{}\n", false},
		{"other group without Stop", `{"owner": {"PreInvocation": [{"type": "command", "command": "/owner-tool"}]}}` + "\n", false},
		{"other group with Stop", `{"owner": {"Stop": [{"type": "command", "command": "/owner-tool"}]}}` + "\n", true},
		// Antigravity runs groups in name order: this one runs before crosshall's.
		{"disabled later group with Stop", `{"owner": {"enabled": false, "Stop": [{"type": "command", "command": "/owner-tool"}]}}` + "\n", false},
		{"earlier group with Stop", `{"aaa-owner": {"Stop": [{"type": "command", "command": "/owner-tool"}]}}` + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setuptest.New(t, "agy")
			target := filepath.Join(f.Home, ".gemini", "config", "hooks.json")
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte(tc.hooks), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := realRun(t, scopedService(f), f, Request{Offer: true, Terminal: true}, "\n")
			if err != nil || strings.Contains(out, line) != tc.want {
				t.Fatalf("offer: %v\n%s", err, out)
			}
		})
	}
}
