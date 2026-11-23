package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
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
		{"whitespace at EOF", " ", telemetryconfig.Unset, 1},
		{"yes at EOF", "yes", telemetryconfig.Unset, 1},
		{"one retry", "typo\nno\n", telemetryconfig.Off, 2},
		{"two invalid answers", "typo\nwrong\n", telemetryconfig.Unset, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			mode, err := askTelemetry(bufio.NewReader(strings.NewReader(tt.input)), &out)
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
		{name: "crosshall environment opt-out", args: []string{"owner", "start"}, configure: func(t *testing.T, _ *Env) {
			t.Setenv(telemetryconfig.TelemetryEnv, "off")
		}},
		{name: "managed opt-out", args: []string{"owner", "start"}, configure: func(t *testing.T, e *Env) {
			if err := os.WriteFile(e.ResolveTelemetryPolicyPath(telemetryconfig.ManagedOffPath), []byte("managed"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no endpoint", args: []string{"owner", "start"}, configure: func(_ *testing.T, e *Env) { e.TelemetryEndpoint = "" }},
		{name: "agent installer", args: []string{"setup", "--offer"}, configure: func(_ *testing.T, e *Env) {
			e.OwnerGate = func() string { return "agent session" }
		}},
		{name: "explicit telemetry off", args: []string{"owner", "telemetry", "off"}, mode: "off"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CI", "")
			if err := os.Unsetenv("CI"); err != nil {
				t.Fatal(err)
			}
			t.Setenv(telemetryconfig.TelemetryEnv, "")
			t.Setenv(telemetryconfig.DoNotTrackEnv, "")
			env := Env{
				Stdin:                      strings.NewReader("\nno\n"),
				SetupTerminal:              func() bool { return true },
				OwnerGate:                  func() string { return "" },
				TelemetryEndpoint:          "https://example.invalid",
				TelemetryStateRoot:         filepath.Join(t.TempDir(), "state"),
				ResolveTelemetryPolicyPath: temporaryPolicyPath(t, filepath.Join(t.TempDir(), "telemetry-off")),
				StartReady:                 func(ctx context.Context) error { return ctx.Err() },
				Setup: func(req SetupRequest) error {
					input := bufio.NewReader(req.Input)
					_, _ = input.ReadString('\n')
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

func TestTelemetryPolicyFileOverridesExplicitOn(t *testing.T) {
	t.Setenv(telemetryconfig.TelemetryEnv, "")
	t.Setenv(telemetryconfig.DoNotTrackEnv, "")
	root := t.TempDir()
	policy := filepath.Join(root, "telemetry-off")
	for _, present := range []bool{false, true} {
		if present {
			if err := os.WriteFile(policy, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		env := Env{TelemetryStateRoot: filepath.Join(t.TempDir(), "state"), ResolveTelemetryPolicyPath: temporaryPolicyPath(t, policy)}
		_, stderr, code := run(t, env, "owner", "telemetry", "on")
		setting, err := telemetryconfig.ReadMode(env.TelemetryStateRoot)
		if err != nil {
			t.Fatal(err)
		}
		if present {
			if code != core.ExitRefused || setting.Mode != telemetryconfig.Unset || !strings.Contains(stderr, "administrator policy") {
				t.Fatalf("present policy: code=%d, mode=%s, stderr=%s", code, setting.Mode, stderr)
			}
		} else if code != core.ExitOK || setting.Mode != telemetryconfig.On {
			t.Fatalf("absent policy: code=%d, mode=%s, stderr=%s", code, setting.Mode, stderr)
		}
	}
}

func TestTelemetryDurableVetoForOwnerHelpAndDryRun(t *testing.T) {
	for _, key := range []string{telemetryconfig.TelemetryEnv, telemetryconfig.DoNotTrackEnv} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(telemetryconfig.TelemetryEnv, "")
			t.Setenv(telemetryconfig.DoNotTrackEnv, "")
			t.Setenv(key, "1")
			if key == telemetryconfig.TelemetryEnv {
				t.Setenv(key, "off")
			}
			env := Env{
				TelemetryStateRoot: filepath.Join(t.TempDir(), "state"),
				Setup:              func(SetupRequest) error { return nil },
			}
			if _, _, err := telemetryconfig.SetMode(env.TelemetryStateRoot, telemetryconfig.On, "owner-command", ""); err != nil {
				t.Fatal(err)
			}
			checkMode := func(want string) {
				t.Helper()
				setting, err := telemetryconfig.ReadMode(env.TelemetryStateRoot)
				if err != nil || setting.Mode != want {
					t.Fatalf("mode=%s, want %s, error=%v", setting.Mode, want, err)
				}
			}
			if _, stderr, code := run(t, env, "setup", "--dry-run"); code != core.ExitOK || stderr != "" {
				t.Fatalf("dry run code=%d, stderr=%s", code, stderr)
			}
			checkMode(telemetryconfig.On)
			if _, stderr, code := run(t, env, "owner", "help"); code != core.ExitOK || !strings.Contains(stderr, "turned off") {
				t.Fatalf("owner help code=%d, stderr=%s", code, stderr)
			}
			checkMode(telemetryconfig.Off)
		})
	}
}

func temporaryPolicyPath(t *testing.T, fixture string) func(string) string {
	t.Helper()
	return func(defaultPath string) string {
		if defaultPath != "/etc/crosshall/telemetry-off" {
			t.Fatalf("default telemetry policy path = %q", defaultPath)
		}
		return fixture
	}
}

func TestTelemetryStatusSaysWhenCurlIsMissing(t *testing.T) {
	t.Setenv(telemetryconfig.TelemetryEnv, "")
	t.Setenv(telemetryconfig.DoNotTrackEnv, "")
	for curl, missing := range map[string]bool{"/nonexistent/curl": true, os.Args[0]: false} {
		env := Env{TelemetryStateRoot: filepath.Join(t.TempDir(), "state"), TelemetryCurl: curl,
			ResolveTelemetryPolicyPath: temporaryPolicyPath(t, filepath.Join(t.TempDir(), "telemetry-off"))}
		stdout, stderr, code := run(t, env, "owner", "telemetry", "on")
		said := strings.Contains(stdout, "Nothing is sent: reports are sent with "+curl+", which is not installed.")
		if code != core.ExitOK || said != missing {
			t.Fatalf("curl %s: code=%d, stdout=%s, stderr=%s", curl, code, stdout, stderr)
		}
	}
}

// show prints the batch the dispatcher sends next byte for byte, and the
// status names the last answer and the next attempt.
func TestTelemetryShowAndRetryStatus(t *testing.T) {
	t.Setenv(telemetryconfig.TelemetryEnv, "")
	t.Setenv(telemetryconfig.DoNotTrackEnv, "")
	env := Env{TelemetryStateRoot: filepath.Join(t.TempDir(), "state"),
		ResolveTelemetryPolicyPath: temporaryPolicyPath(t, filepath.Join(t.TempDir(), "telemetry-off"))}
	if _, _, code := run(t, env, "owner", "telemetry", "on"); code != core.ExitOK {
		t.Fatal(code)
	}
	if _, stderr, code := run(t, env, "owner", "telemetry", "show"); code != core.ExitUnavailable ||
		!strings.Contains(stderr, "no usage report is waiting to be sent") {
		t.Fatalf("show with nothing pending: code=%d, stderr=%s", code, stderr)
	}
	body := `{"v":1,"registry":1,"batch":"00000000000000000000000000000000","day":"2026-11-09"}`
	next := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	dir, err := os.OpenRoot(filepath.Join(env.TelemetryStateRoot, "telemetry"))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	st := &telemetryconfig.State{LastStatus: "429", Days: map[string]*telemetryconfig.DayState{
		"2026-11-09": {Batch: true, NotBefore: next, Attempts: 1}}}
	if err := errors.Join(telemetryconfig.WriteDayFile(dir, "pending", "2026-11-09", []byte(body)),
		telemetryconfig.WriteState(dir, st)); err != nil {
		t.Fatal(err)
	}
	if stdout, _, code := run(t, env, "owner", "telemetry", "show"); code != core.ExitOK || stdout != body {
		t.Fatalf("show: code=%d, stdout=%q", code, stdout)
	}
	stdout, _, code := run(t, env, "owner", "telemetry")
	if code != core.ExitOK || !strings.Contains(stdout, "Last answer from the receiver: 429") ||
		!strings.Contains(stdout, "Waiting to be sent: the report for 2026-11-09, not before 2026-11-10T03:00:00Z") {
		t.Fatalf("status: code=%d, stdout=%s", code, stdout)
	}
	stdout, _, code = run(t, env, "--json", "owner", "telemetry")
	var status struct {
		LastAnswer  string    `json:"last_answer"`
		PendingDay  string    `json:"pending_day"`
		NextAttempt time.Time `json:"next_attempt"`
	}
	if err := json.Unmarshal([]byte(stdout), &status); code != core.ExitOK || err != nil ||
		status.LastAnswer != "429" || status.PendingDay != "2026-11-09" || !status.NextAttempt.Equal(next) {
		t.Fatalf("status json: code=%d, %s, %v", code, stdout, err)
	}
}
