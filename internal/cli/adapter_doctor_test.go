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
				"adapters":[{"adapter":"claude","executable":"/bin/claude","version":"2.1.327",
				"install_state":"installed","installed":5,"intended":6,"drifted":1,"trust":"unknown",
				"policy":{"enabled":true,"reminder":false,"epoch":7},
				"evidence":[{"version":"2.1.327","platform":"darwin-arm64","mode":"print",
				"manifest_rev":"claude-1","capability":"enroll","level":"tested"}],
				"unverified_launches":2,
				"hooks":{"date":"20261119","p95_ms":10,"counts":{"clean":2,"rpc":1}}}]
			}`)}
		},
	}
	out, _, code := run(t, env, "owner", "doctor")
	if code != core.ExitOK {
		t.Fatalf("doctor diagnostics exit %d: %s", code, out)
	}
	for _, want := range []string{"/bin/claude", "2.1.327", "5/6 installed", "1 drifted", "epoch 7",
		"darwin-arm64", "print", "enroll tested", "2 live launches with unverified version or mode", "p95 10 ms", "clean=2 rpc=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	out, _, code = run(t, env, "--json", "owner", "doctor")
	var data struct {
		Adapters []struct {
			Adapter string
			Hooks   json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(out), &data); code != core.ExitOK || err != nil ||
		len(data.Adapters) != 1 || data.Adapters[0].Adapter != "claude" || len(data.Adapters[0].Hooks) == 0 {
		t.Fatalf("doctor diagnostics JSON exit %d: %s (%v)", code, out, err)
	}
}
