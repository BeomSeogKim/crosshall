package setup

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestCodexSetupRefusalNamesOriginalState(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "pinned"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			f := setuptest.New(t, "codex")
			original := scopedService(f)
			if !legacy {
				original.StateRoot = filepath.Join(f.Root, "owner's original state")
			}
			plan := codexInstallPlan(t, original)
			if plan.Err != nil {
				t.Fatal(plan.Err)
			}
			want := "CROSSHALL_STATE_DIR=" + shellQuote(original.StateRoot) + " crosshall setup codex"
			if legacy {
				writeLegacyCodex(t, f, original, []byte("{}\n"), true)
				want = "CROSSHALL_STATE_DIR='<original-state-directory>' crosshall uninstall codex"
			} else {
				f.CheckTargets(t)
				if err := original.Install(t.Context(), plan, ClaudeRules{}); err != nil {
					t.Fatal(err)
				}
			}
			before := configBytes(t, plan.Path)
			other := original
			other.StateRoot = filepath.Join(f.Root, "wrong state")
			for _, call := range []struct{ apply, json bool }{{true, false}, {true, true}, {false, false}} {
				var out bytes.Buffer
				req := Request{Action: "setup", Adapters: []string{"codex"}, Apply: call.apply, JSON: call.json, Output: &out}
				err := other.Run(t.Context(), req)
				if err == nil {
					t.Fatal("setup from the unrelated state succeeded")
				}
				if !strings.Contains(err.Error(), want) {
					t.Errorf("failure summary did not use the next step %q: %v", want, err)
				}
				if call.json {
					var result promptResult
					if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.OK || len(result.Vendors) != 1 || result.Vendors[0].Retry != want {
						t.Errorf("JSON refusal did not point to %q: %s (%v)", want, out.String(), err)
					}
				} else {
					if !strings.Contains(out.String(), "next: "+want) {
						t.Errorf("text refusal did not point to %q: %s", want, out.String())
					}
					if strings.Contains(out.String(), "To install without questions") {
						t.Errorf("refused preview still offered installation from this state: %s", out.String())
					}
				}
			}
			if got := configBytes(t, plan.Path); !bytes.Equal(got, before) {
				t.Fatal("refusal changed the installed hooks")
			}
			if _, err := os.Stat(other.StateRoot); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote to the unrelated state: %v", err)
			}
			f.CheckTargets(t)
			if legacy {
				plans, err := original.PlanUninstall(t.Context(), []string{"codex"})
				if err != nil || len(plans) != 1 {
					t.Fatalf("original-root uninstall plan: %v, %v", plans, err)
				}
				if err := original.Uninstall(t.Context(), plans[0]); err != nil {
					t.Fatalf("original-root uninstall: %v", err)
				}
			} else if err := original.Run(t.Context(), Request{Action: "setup", Adapters: []string{"codex"}, Apply: true, Output: io.Discard}); err != nil {
				t.Fatalf("original-root setup: %v", err)
			}
		})
	}
}
