package setup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func TestInstallerConsentDisclosesOnlyTheApplicableEffects(t *testing.T) {
	const noticeOn = "- Notices (Claude Code, interactive sessions). When work arrives for an idle session, the hall starts a turn in it. Each notice costs that session one model turn. Off: crosshall owner publish hall off"
	const probation = " This version is new to crosshall: the first three notices are checked, and a bad one stops notices until you resume them."
	const turnEndOn = "- Turn-end check (Claude Code). A turn that ends while a request is unread continues once. Each continuation costs one model request. Off: only with the adapter, crosshall owner adapter set claude --enabled off"
	const unavailable = "- Notices and the turn-end check are not available for these builds. A session sees new work at its next prompt."
	for _, tc := range []struct {
		name, version, publishLevel, turnEndLevel, settings, notice, turnEnd string
	}{
		{name: "exact", version: "2.1.327", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn, turnEnd: turnEndOn},
		{name: "borrowed", version: "2.1.326", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn + probation, turnEnd: turnEndOn},
		{name: "seen", version: "2.1.326", publishLevel: "tested", turnEndLevel: "tested", notice: noticeOn, turnEnd: turnEndOn,
			settings: `INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, confirmed)
				VALUES ('claude', '2.1.327', ?1, 'interactive', 'claude-2', 'publish', 3)`},
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
				VALUES ('claude', '2.1.327', ?1, 'interactive', 'claude-2', 'publish', 1, 'absorbed', 41)`,
			notice: "- Notices (Claude Code, interactive sessions). Stopped for this version after a notice went wrong. crosshall owner who --all shows how to resume."},
		{name: "exact candidate", version: "2.1.327", publishLevel: "candidate", turnEndLevel: "candidate", notice: unavailable},
		{name: "publish unavailable", version: "2.1.327", publishLevel: "candidate", turnEndLevel: "tested", turnEnd: turnEndOn,
			notice: "- Notices. Not available for these builds: a session sees new work at its next prompt."},
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
				err = db.Update(ctx, func(tx store.Tx) error {
					_, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
						(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms) VALUES
						('claude', ?2, ?1, 'interactive', 'claude-2', 'publish', ?3, 'fixture', 1),
						('claude', ?2, ?1, 'interactive', 'claude-2', 'turn_end', ?4, 'fixture', 1),
						('claude', ?2, ?1, 'print', 'claude-2', 'turn_end', ?4, 'fixture', 1),
						('claude', '2.1.325', ?1, 'interactive', 'claude-2', 'publish', 'tested', 'older', 1)`,
						platform, tc.version, tc.publishLevel, tc.turnEndLevel)
					if err == nil && tc.settings != "" {
						_, err = tx.ExecContext(ctx, tc.settings, platform)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			offer := tc.name != "owner adapter off" && tc.name != "owner adapter on" && tc.name != "unreadable"
			out, err := realRun(t, s, f, Request{Offer: offer, Terminal: true}, "\n")
			if err != nil {
				t.Fatal(err)
			}
			_, block, found := strings.Cut(out, "Saying yes turns these on for the listed agents:\n")
			end := "Install the listed hooks and command rules?"
			if offer {
				end = "A no skips future installer offers"
			}
			block, _, ends := strings.Cut(block, end)
			joining := "- Joining. A new session joins its repository's channel when it starts. A Git repository with no channel gets one, named after its directory. Off for repositories without a channel: crosshall owner channel auto off. A repository that has a channel keeps joining its sessions while the agent's hooks are on."
			if tc.name == "owner hall off" {
				joining = "- Joining. A new session joins its repository's channel when it starts, in repositories that have one. You turned automatic joining off, so a repository without a channel gets none. On: crosshall owner channel auto on"
			} else if tc.name == "unreadable" {
				joining = "- Joining. A new session joins its repository's channel when it starts, in repositories that have one. Whether a repository without a channel gets one depends on your setting (crosshall owner channel auto on|off)."
			}
			want := joining + "\n- Prompt line. A joined session sees one line with the numbers of its unread messages at its next prompt. It costs no model request.\n" + notice + "\n"
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
