package setup

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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

func TestInstallerPromptDisclosureMatchesTheListedAgent(t *testing.T) {
	for _, tc := range []struct {
		adapter string
		prompt  bool
	}{{"codex", true}, {"agy", false}} {
		t.Run(tc.adapter, func(t *testing.T) {
			f := setuptest.New(t, tc.adapter)
			out, err := realRun(t, scopedService(f), f, Request{Offer: true, Terminal: true}, "\n")
			pickup := "its next crosshall read."
			if tc.prompt {
				pickup = "its next prompt or " + pickup
			}
			if err != nil || strings.Contains(out, "- Prompt line (Claude Code, Codex CLI).") != tc.prompt ||
				!strings.Contains(out, "- Notices and the turn-end check are not available for these builds. A session sees new work at "+pickup) {
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
			if err != nil || !strings.Contains(out, tc.notice+"\n"+turnEnd+"\nA no skips future installer offers") ||
				strings.Contains(out, "not available for these builds") {
				t.Fatalf("offer: %v\n%s", err, out)
			}
		})
	}
}

func TestInstallerConsentDisclosesOnlyTheApplicableEffects(t *testing.T) {
	const noticeOn = "- Notices (Claude Code, interactive sessions). When work arrives for an idle session, the hall starts a turn in it. Each notice costs that session one model turn. Off: crosshall owner publish hall off"
	const probation = " This version is new to crosshall: the first three notices are checked, and a bad one stops notices until you resume them."
	const turnEndBody = "When a turn ends while a request waits that is unread, unanswered and not yet announced to the session, the turn continues once. Each continuation costs one model request. Off: crosshall owner adapter set claude --turn-end off"
	const turnEndOn = "- Turn-end check (Claude Code, interactive sessions and print mode). " + turnEndBody
	const unavailable = "- Notices and the turn-end check are not available for these builds. A session sees new work at its next prompt or its next crosshall read."
	for _, tc := range []struct {
		name, version, publishLevel, turnEndLevel, printLevel, settings, notice, turnEnd string
	}{
		{name: "exact", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn, turnEnd: turnEndOn},
		{name: "print only", version: "2.1.327", publishLevel: "tested", turnEndLevel: "unavailable", printLevel: "tested",
			notice: noticeOn, turnEnd: "- Turn-end check (Claude Code, print mode). " + turnEndBody},
		{name: "interactive only", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", printLevel: "unavailable",
			notice: noticeOn, turnEnd: "- Turn-end check (Claude Code, interactive sessions). " + turnEndBody},
		{name: "borrowed", version: "2.1.326", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn + probation, turnEnd: turnEndOn},
		{name: "seen", version: "2.1.326", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn, turnEnd: turnEndOn,
			settings: `INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, confirmed)
				VALUES ('claude', '2.1.327', ?1, 'interactive', 'claude-3', 'publish', 3)`},
		{name: "owner hall off", version: "2.1.327", publishLevel: "tested", turnEndLevel: "candidate",
			settings: `INSERT INTO hall_publish VALUES (1, 0, 1); INSERT INTO hall_auto_join VALUES (1, 0, 1)`,
			notice:   "- Notices (Claude Code, interactive sessions). Off by your setting; installing does not change it. On: crosshall owner publish hall on",
			turnEnd:  "- Turn-end check. Not available for these builds."},
		{name: "owner adapter off", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", turnEnd: turnEndOn,
			settings: `INSERT INTO hall_publish VALUES (1, 0, 1);
				INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, publish) VALUES ('claude', 0, 0, 1, 1, 0)`,
			notice: "- Notices (Claude Code, interactive sessions). Off by your setting for Claude Code; installing does not change it. On: crosshall owner adapter set claude --publish on"},
		{name: "owner adapter on", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", turnEnd: turnEndOn,
			settings: `INSERT INTO hall_publish VALUES (1, 0, 1);
				INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms, publish) VALUES ('claude', 0, 0, 1, 1, 1)`,
			notice: strings.Replace(noticeOn, "crosshall owner publish hall off", "crosshall owner adapter set claude --publish off", 1)},
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
			turnEnd:  "- Turn-end check. Not available for these builds."},
		{name: "exact candidate", version: "2.1.327", publishLevel: "candidate", turnEndLevel: "candidate", notice: unavailable},
		{name: "publish unavailable", version: "2.1.327", publishLevel: "candidate", turnEndLevel: "tested", turnEnd: turnEndOn,
			notice: "- Notices. Not available for these builds: a session sees new work at its next prompt or its next crosshall read."},
		{name: "cold"},
		{name: "unreadable", notice: "- Notices and the turn-end check. crosshall could not read its settings, so it cannot say here whether they are on for these builds. After installing, crosshall owner doctor shows it."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, _ := realSetup(t)
			ctx := t.Context()
			platform := runtime.GOOS + "-" + runtime.GOARCH
			notice, turnEnd := tc.notice, tc.turnEnd
			ledger := filepath.Join(f.State, "store.sqlite3")
			if tc.name == "cold" {
				notice = unavailable
				if platform == "darwin-arm64" {
					notice, turnEnd = noticeOn+probation, turnEndOn
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
				err = db.Update(ctx, func(tx store.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
						(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms) VALUES
						('claude', ?2, ?1, 'interactive', 'claude-3', 'publish', ?3, 'fixture', 1),
						('claude', ?2, ?1, 'interactive', 'claude-3', 'turn_end', ?4, 'fixture', 1),
						('claude', ?2, ?1, 'print', 'claude-3', 'turn_end', ?5, 'fixture', 1),
						('claude', '2.1.325', ?1, 'interactive', 'claude-3', 'publish', 'tested', 'older', 1)`,
						platform, tc.version, tc.publishLevel, tc.turnEndLevel, printLevel)
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
			req := Request{Offer: offer, Terminal: true}
			var out string
			var err error
			var terminal readmeTerminal
			if tc.name == "exact" {
				req.PathConsent = func(bool) error { return nil }
				err = runRealPrompts(t, s, f, req, &terminal, &terminal)
				out = terminal.String()
			} else {
				out, err = realRun(t, s, f, req, "\n")
			}
			if err != nil {
				t.Fatal(err)
			}
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
			want := joining + "\n- Prompt line (Claude Code, Codex CLI). A joined session sees one line with the numbers of its unread messages at its next prompt, and crosshall read picks them up. The line itself costs no model request.\n" + notice + "\n"
			if turnEnd != "" {
				want += turnEnd + "\n"
			}
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
	stub := "#!/bin/sh\n[ \"$1\" = --version ] || exit 2\nprintf '%s\\n' '2.1.330 (Claude Code)'\n"
	if err := os.WriteFile(filepath.Join(f.Bin, "claude"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := realRun(t, s, f, Request{Offer: true, Terminal: true}, "\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.State, "store.sqlite3")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the preview created a store: %v", err)
	}
	// The Claude Code entry runs from its own line to the next unindented one.
	_, block, _ := strings.Cut(out, "\nclaude 2.1.330 ")
	if i := strings.Index(block, "\n"); i >= 0 {
		block = block[i+1:]
	}
	for i, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "  ") {
			block = strings.Join(strings.Split(block, "\n")[:i], "\n") + "\n"
			break
		}
	}
	listed := strings.Contains(block, "  publish darwin-arm64/interactive tested\n") &&
		strings.Contains(block, "  turn_end darwin-arm64/interactive tested\n") &&
		strings.Contains(block, "  turn_end darwin-arm64/print tested\n")
	unknown := strings.Contains(block, "Capability evidence for this build: unknown")
	if runtime.GOOS+"-"+runtime.GOARCH == "darwin-arm64" {
		if !listed || unknown || !strings.Contains(out, "When work arrives for an idle session") {
			t.Fatalf("the listing and the question disagree:\n%s", out)
		}
	} else if listed || !unknown {
		t.Fatalf("listed built-in evidence for another platform:\n%s", out)
	}
}
