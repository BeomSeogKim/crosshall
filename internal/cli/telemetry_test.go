package cli

import (
	"bufio"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/setuptest"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

func TestTelemetryQuestion(t *testing.T) {
	wantQuestion := setuptest.ReadmeBlock(t, "telemetry-question")
	for _, tt := range []struct {
		name, input, mode string
		questions         int
	}{
		{"default after Enter", "\n", telemetryconfig.On, 1},
		{"typed yes", "YeS\n", telemetryconfig.On, 1},
		{"decline", "n\n", telemetryconfig.Off, 1},
		{"EOF", "", telemetryconfig.Unset, 1},
		{"one retry", "typo\nno\n", telemetryconfig.Off, 2},
		{"two invalid answers", "typo\nwrong\n", telemetryconfig.Unset, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			mode, err := askTelemetry(bufio.NewScanner(strings.NewReader(tt.input)), &out)
			if err != nil || mode != tt.mode || out.String() != strings.Repeat(wantQuestion, tt.questions) {
				t.Fatalf("mode=%q, error=%v, question=%q", mode, err, out.String())
			}
		})
	}
}

func TestTelemetryOfferAtOwnerEntryPoints(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		configure func(*testing.T, *Env)
		mode      string
		source    string
	}{
		{name: "first owner command", args: []string{"owner", "start"}, mode: "on", source: "owner-prompt"},
		{name: "installer offer", args: []string{"setup", "--offer"}, mode: "off", source: "installer"},
		{name: "plain setup", args: []string{"setup"}, mode: "off", source: "owner-prompt"},
		{name: "JSON", args: []string{"--json", "owner", "start"}},
		{name: "no terminal", args: []string{"owner", "start"}, configure: func(_ *testing.T, e *Env) {
			e.SetupTerminal = func() bool { return false }
		}},
		{name: "environment opt-out", args: []string{"owner", "start"}, configure: func(t *testing.T, _ *Env) {
			t.Setenv(telemetryconfig.DoNotTrackEnv, "1")
		}},
		{name: "managed opt-out", args: []string{"owner", "start"}, configure: func(_ *testing.T, e *Env) {
			e.TelemetryManagedOff = func() bool { return true }
		}},
		{name: "no endpoint", args: []string{"owner", "start"}, configure: func(_ *testing.T, e *Env) { e.TelemetryEndpoint = "" }},
		{name: "agent installer", args: []string{"setup", "--offer"}, configure: func(_ *testing.T, e *Env) {
			e.OwnerGate = func() string { return "agent session" }
		}},
		{name: "explicit telemetry off", args: []string{"owner", "telemetry", "off"}, mode: "off"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(telemetryconfig.TelemetryEnv, "")
			t.Setenv(telemetryconfig.DoNotTrackEnv, "")
			env := Env{
				Stdin:              strings.NewReader("\nno\n"),
				SetupTerminal:      func() bool { return true },
				OwnerGate:          func() string { return "" },
				TelemetryEndpoint:  "https://example.invalid",
				TelemetryStateRoot: filepath.Join(t.TempDir(), "state"),
				StartReady:         func(ctx context.Context) error { return ctx.Err() },
				Setup: func(req SetupRequest) error {
					input := bufio.NewScanner(req.Input)
					input.Scan()
					if req.OfferTelemetry != nil {
						return req.OfferTelemetry(input)
					}
					return nil
				},
			}
			if tt.source != "" {
				env.CommandDeadline = time.Now().Add(-time.Second)
			}
			if tt.configure != nil {
				tt.configure(t, &env)
			}
			stdout, stderr, code := run(t, env, tt.args...)
			if code != core.ExitOK {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			setting, err := telemetryconfig.ReadMode(env.TelemetryStateRoot)
			wantMode := tt.mode
			if wantMode == "" {
				wantMode = telemetryconfig.Unset
			}
			if err != nil || setting.Mode != wantMode || (tt.source != "" && setting.Source != tt.source) {
				t.Fatalf("setting=%+v, error=%v, want %s (%s)", setting, err, wantMode, tt.source)
			}
			if asked := strings.Contains(stdout, telemetryQuestion); asked != (tt.source != "") {
				t.Fatalf("unexpected question state, asked=%v: %s", asked, stdout)
			}
			if tt.source != "" {
				env.CommandDeadline = time.Time{}
				stdout, stderr, code = run(t, env, tt.args...)
				if code != core.ExitOK || strings.Contains(stdout, telemetryQuestion) {
					t.Fatalf("second invocation exit=%d, stdout=%s, stderr=%s", code, stdout, stderr)
				}
			}
		})
	}
}
