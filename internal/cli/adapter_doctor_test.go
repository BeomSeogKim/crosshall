package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestDoctorRendersAdapterDiagnostics(t *testing.T) {
	env := Env{
		OwnerGate: func() string { return "" },
		Doctor: func(context.Context) core.Result {
			return core.Result{Status: core.ExitOK, Data: json.RawMessage(`{
				"checks": [{"name":"lock","ok":true,"detail":"dispatcher lock held"}],
				"adapters":[{"adapter":"claude","executable":"/bin/claude","version":"9.1.42",
				"install_state":"installed","installed":5,"intended":6,"drifted":1,"trust":"unknown",
				"policy":{"enabled":true,"reminder":false,"epoch":7},
				"reminder":{"available":false,"release_state":"parked","reason":"The release has no active reminder path.",
					"extra_model_requests":1,"provenance":"Fixed crosshall text sent to the model.",
					"evidence":[{"version":"9.1.42","platform":"darwin-arm64","mode":"print","manifest_rev":"claude-1",
						"level":"candidate","reason":"The native tool-call ID was observed."}]},
				"evidence":[{"version":"2.1.327","platform":"darwin-arm64","mode":"print",
				"manifest_rev":"claude-1","capability":"enroll","level":"tested"}],
				"turn_end_notice":{"available":true,"date":"20261119","committed_claims":2,
					"extra_model_requests_per_continuation":1,"trusted_from":"2.1.330","live_launches":3},
				"publish":{"builds":[
					{"version":"2.1.330","platform":"darwin-arm64","mode":"interactive","manifest_rev":"claude-2","state":"tested"},
					{"version":"2.1.331","platform":"darwin-arm64","mode":"interactive","manifest_rev":"claude-2","state":"trusted","confirmed":1,"held_unobserved":2},
					{"version":"2.1.332","platform":"darwin-arm64","mode":"interactive","manifest_rev":"claude-2","state":"seen"},
					{"version":"2.1.333","platform":"darwin-arm64","mode":"interactive","manifest_rev":"claude-2","state":"stopped","stopped_at_ms":60000,"notice":41,"reason":"absorbed"}]},
				"unverified_launches":2,"unseen_starts":1,
				"hooks":{"date":"20261119","p95_ms":150,"counts":{"clean":2,"deadline":2,"rpc":1},
					"recorded_starts":5,"helper_timeouts":2,"missed_deadlines":1,"vendor_timeouts":null}},
				{"adapter":"codex","intended":5,"policy":{"enabled":true,"reminder":false,"turn_end":"off","epoch":1},"reminder":{"available":false,"release_state":"parked",
					"reason":"The release has no active reminder path.","extra_model_requests":1,
					"provenance":"Fixed crosshall text sent to the model.","evidence":[{"level":"unknown"}]},
				 "turn_end_notice":{"available":false,"date":"20261119","committed_claims":0,
					"extra_model_requests_per_continuation":1}}]
			}`)}
		},
	}
	out, _, code := run(t, env, "owner", "doctor")
	if code != core.ExitOK {
		t.Fatalf("doctor diagnostics exit %d: %s", code, out)
	}
	for _, want := range []string{"/bin/claude", "9.1.42", "5/6 installed", "1 drifted", "policy on, reminder off, epoch 7",
		"reply reminder parked (no opt-in)",
		`reply reminder evidence candidate for "9.1.42" "darwin-arm64" "print" "claude-1" — The native tool-call ID was observed.`,
		"reply reminder evidence unknown\n",
		"extra model requests per reminder 1 — Fixed crosshall text sent to the model.",
		"turn-end notice available (trusted from 2.1.330 evidence), committed claims 2 on 20261119 UTC, 1 extra model request per continuation",
		"turn-end notice off by the owner (undo: crosshall owner adapter set codex --turn-end default), committed claims 0 on 20261119 UTC, 1 extra model request per continuation",
		"publish build 2.1.330 (darwin-arm64 interactive claude-2): tested",
		"publish build 2.1.331 (darwin-arm64 interactive claude-2): trusted 1 of 3, 2 held unobserved",
		"publish build 2.1.332 (darwin-arm64 interactive claude-2): seen",
		"publish build 2.1.333 (darwin-arm64 interactive claude-2): stopped at " + clock(60000) + " (notice 41, absorbed)",
		"darwin-arm64", "print", "enroll tested", "2 live launches with unverified version or mode",
		"live members without a notice route because their session start was not seen: 1",
		"p95 150 ms", "clean=2 deadline=2 rpc=1",
		"recorded starts 5, helper timeouts 2, missed deadlines 1, vendor timeouts unknown", "hook counters unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	out, _, code = run(t, env, "--json", "owner", "doctor")
	var data struct {
		Adapters []struct {
			Adapter string
			Hooks   json.RawMessage
			Publish struct {
				Builds []struct {
					HeldUnobserved *int `json:"held_unobserved"`
				}
			}
			TurnEndNotice *struct {
				Available                         bool
				Date                              string
				TrustedFrom                       string `json:"trusted_from"`
				CommittedClaims                   int    `json:"committed_claims"`
				ExtraModelRequestsPerContinuation int    `json:"extra_model_requests_per_continuation"`
			} `json:"turn_end_notice"`
			Reminder *struct {
				Available          *bool
				Reason, Provenance string
				ReleaseState       string `json:"release_state"`
				ExtraModelRequests int    `json:"extra_model_requests"`
				Evidence           []struct {
					Version, Platform, Mode, Level, Reason string
					ManifestRev                            string `json:"manifest_rev"`
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &data); code != core.ExitOK || err != nil ||
		len(data.Adapters) != 2 || data.Adapters[0].Adapter != "claude" || len(data.Adapters[0].Hooks) == 0 ||
		data.Adapters[1].Adapter != "codex" || len(data.Adapters[1].Hooks) != 0 {
		t.Fatalf("doctor diagnostics JSON exit %d: %s (%v)", code, out, err)
	}
	builds := data.Adapters[0].Publish.Builds
	if len(builds) != 4 || builds[1].HeldUnobserved == nil || *builds[1].HeldUnobserved != 2 ||
		builds[0].HeldUnobserved != nil || builds[2].HeldUnobserved != nil || builds[3].HeldUnobserved != nil {
		t.Errorf("unexpected held-unobserved count in doctor JSON: %s", out)
	}
	if r := data.Adapters[0].Reminder; r == nil || r.Available == nil || *r.Available ||
		r.ReleaseState != "parked" || r.Reason != "The release has no active reminder path." ||
		r.ExtraModelRequests != 1 || r.Provenance != "Fixed crosshall text sent to the model." {
		t.Errorf("Claude reminder status: %+v", r)
	} else if len(r.Evidence) != 1 || r.Evidence[0].Level != "candidate" ||
		r.Evidence[0].Version != "9.1.42" || r.Evidence[0].Platform != "darwin-arm64" ||
		r.Evidence[0].Mode != "print" || r.Evidence[0].ManifestRev != "claude-1" ||
		r.Evidence[0].Reason != "The native tool-call ID was observed." {
		t.Errorf("Claude reminder evidence: %+v", r.Evidence)
	}
	if r := data.Adapters[1].Reminder; r == nil || len(r.Evidence) != 1 ||
		r.Evidence[0].Level != "unknown" || r.Evidence[0].Mode != "" {
		t.Errorf("Codex reminder evidence: %+v", r)
	}
	for i, wantClaims := range []int{2, 0} {
		a := data.Adapters[i]
		if n := a.TurnEndNotice; n == nil || n.Date != "20261119" || n.ExtraModelRequestsPerContinuation != 1 ||
			n.Available != (i == 0) || n.CommittedClaims != wantClaims || (i == 0 && n.TrustedFrom != "2.1.330") {
			t.Errorf("%s turn-end notice: %+v", a.Adapter, n)
		}
	}
}

// With no live launch, doctor has checked no build: it says so instead of
// "unavailable", which a new user reads as broken.
func TestDoctorSaysTheTurnEndNoticeIsUncheckedWithoutALiveSession(t *testing.T) {
	env := Env{
		OwnerGate: func() string { return "" },
		Doctor: func(context.Context) core.Result {
			return core.Result{Status: core.ExitOK, Data: json.RawMessage(`{"checks":[{"name":"lock","ok":true,"detail":"dispatcher lock held"}],"adapters":[
				{"adapter":"claude","policy":{"enabled":true,"epoch":1},"turn_end_notice":{"available":false,"date":"20261127",
					"committed_claims":0,"extra_model_requests_per_continuation":1,"live_launches":0}},
				{"adapter":"codex","policy":{"enabled":true,"epoch":1},"turn_end_notice":{"available":false,"date":"20261127",
					"committed_claims":0,"extra_model_requests_per_continuation":1,"live_launches":1}}]}`)}
		},
	}
	out, _, code := run(t, env, "owner", "doctor")
	for _, want := range []string{
		"turn-end notice not checked yet: no live Claude Code session, committed claims 0 on 20261127 UTC",
		"turn-end notice unavailable, committed claims 0 on 20261127 UTC",
	} {
		if code != core.ExitOK || !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}
