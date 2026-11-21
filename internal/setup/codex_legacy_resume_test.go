package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestResumedLegacyCodexInstallReportsPinFollowup(t *testing.T) {
	for _, tc := range []struct {
		name string
		json bool
	}{
		{"noninteractive text", false},
		{"noninteractive JSON", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setuptest.New(t, "codex")
			s := scopedService(f)
			op, approved := recordPendingLegacyCodexInstall(t, s, f)
			path := op.Edits[0].Path

			var out bytes.Buffer
			f.CheckTargets(t)
			req := Request{Action: "setup", Apply: true, Adapters: []string{"codex"}, JSON: tc.json,
				Input: strings.NewReader(""), Output: &out}
			if err := s.Run(t.Context(), req); err != nil {
				t.Fatalf("resume failed: %v\n%s", err, out.String())
			}
			resumed := configBytes(t, path)
			if !bytes.Equal(resumed, approved) || strings.Contains(codexSessionStartCommand(t, resumed), "CROSSHALL_STATE_DIR=") {
				t.Fatalf("resume changed the previously approved inherited hooks: %s", resumed)
			}
			if tc.json {
				var result struct {
					OK      bool
					Vendors []struct {
						Outcome string
						Retry   string
						Detail  string
					}
				}
				err := json.Unmarshal(out.Bytes(), &result)
				if err != nil || !result.OK || len(result.Vendors) != 1 ||
					result.Vendors[0].Outcome != "installed" || result.Vendors[0].Retry != "crosshall setup codex" ||
					!strings.Contains(result.Vendors[0].Detail, "preview and apply the state-root pin") {
					t.Errorf("resume JSON did not guide the owner to the pin: %q (%v)", out.String(), err)
				}
			} else if !strings.Contains(out.String(), "next: crosshall setup codex") ||
				!strings.Contains(out.String(), "preview and apply the state-root pin") {
				t.Errorf("resume output did not guide the owner to the pin: %q", out.String())
			}

			plan := codexInstallPlan(t, s)
			if plan.Err != nil || plan.Resume || len(plan.Files) != 2 || plan.Files[1].AddText == "" ||
				len(plan.Files[0].Add) == 0 || len(plan.Files[0].Add) != len(plan.Files[0].Remove) {
				t.Fatalf("next setup did not preview a fresh pin: resume=%t, err=%v", plan.Resume, plan.Err)
			}
			f.CheckTargets(t)
			out.Reset()
			if err := s.Run(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "preview and apply the state-root pin") {
				t.Fatalf("setup advised installing a pin it just installed: %s", out.String())
			}
			pin := "CROSSHALL_STATE_DIR=" + shellQuote(s.StateRoot) + " "
			if command := codexSessionStartCommand(t, configBytes(t, path)); !strings.HasPrefix(command, pin) {
				t.Fatalf("subsequent setup did not apply the pin: %q", command)
			}
		})
	}
}

// The version before the pin feature produced an ordinary Add edit with no
// Restore step. Use the real installer and stop it at the policy update.
func recordPendingLegacyCodexInstall(t *testing.T, s Service, f setuptest.Fixture) (operation, []byte) {
	t.Helper()
	specs, err := selectedSpecs([]string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	j, digest, err := readJournal(s.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.preflightInstall(specs[0], j)
	if err != nil {
		t.Fatal(err)
	}
	target.CommandRules = nil
	for i, entry := range target.Wanted {
		command, err := recordedCodexRoute(entry)
		if err != nil {
			t.Fatal(err)
		}
		pinned, err := json.Marshal(command.command)
		if err != nil {
			t.Fatal(err)
		}
		legacy, err := json.Marshal(shellQuote(s.Executable) + " hook codex " + entry.Event)
		if err != nil {
			t.Fatal(err)
		}
		target.Wanted[i].Value = bytes.Replace(entry.Value, pinned, legacy, 1)
	}
	op, err := makeInstall(j, []installTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	if len(op.Edits) != 1 || len(op.Edits[0].Add) == 0 || len(op.Edits[0].Restore) != 0 {
		t.Fatal("expected an approved pre-pin Add edit")
	}
	approved, err := op.Edits[0].materialize(nil)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupted during policy enable")
	s.SetPolicy = func(context.Context, string, bool) error { return interrupted }
	f.CheckTargets(t)
	if err := s.apply(t.Context(), digest, op); !errors.Is(err, interrupted) {
		t.Fatalf("fixture did not leave a pending legacy install: %v", err)
	}
	j, _, err = readJournal(s.StateRoot)
	if err != nil || j.Pending == nil {
		t.Fatalf("expected a pending legacy journal: %v", err)
	}
	if _, err := os.Stat(op.Edits[0].Path); err != nil {
		t.Fatal(err)
	}
	return op, approved
}
