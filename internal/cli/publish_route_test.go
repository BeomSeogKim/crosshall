package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestWhoRendersPublishCandidateAndReason(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.WhoResult{
		Channel: "dev",
		Members: []domain.Member{{
			Alias:           "fable",
			Vendor:          "claude",
			State:           "unknown",
			LastCommandAgoS: 5,
			Route:           "pull",
			Publish:         &domain.PublishStatus{State: "candidate", Reason: "candidate profile (no sender in slice 1)", Generation: 1},
		}},
	})}
	stdout, _, code := run(t, Env{Transport: ft}, "who")
	if code != core.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "publish: candidate (candidate profile (no sender in slice 1))") {
		t.Fatalf("who output missing publish: candidate:\n%s", stdout)
	}
}

func TestDoctorRendersPublishCandidateAndReason(t *testing.T) {
	data, _ := json.Marshal(struct {
		Checks   []any `json:"checks"`
		Adapters []any `json:"adapters"`
	}{
		Checks: []any{map[string]any{"name": "lock", "ok": true, "detail": "ok"}},
		Adapters: []any{map[string]any{
			"adapter":       "claude",
			"install_state": "installed",
			"intended":      6,
			"installed":     6,
			"publish": map[string]any{
				"state":         "candidate",
				"reason":        "candidate profile (no sender in slice 1)",
				"active_routes": 1,
				"total_routes":  1,
			},
		}},
	})
	env := Env{
		OwnerGate: func() string { return "" },
		Doctor:    func(context.Context) core.Result { return core.Result{Status: core.ExitOK, Data: data} },
	}
	stdout, _, code := run(t, env, "owner", "doctor")
	if code != core.ExitOK {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "publish: candidate (candidate profile (no sender in slice 1), 1 active / 1 total routes)") {
		t.Fatalf("doctor output missing publish: candidate:\n%s", stdout)
	}
}
