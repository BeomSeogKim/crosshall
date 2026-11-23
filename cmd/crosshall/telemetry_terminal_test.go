//go:build testvendorhome

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/setup"
	"github.com/BeomSeogKim/crosshall/internal/setuptest"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

const telemetryPTYMode = "CROSSHALL_TEST_TELEMETRY_PTY_MODE"

func TestTelemetryTerminalChild(t *testing.T) {
	mode := os.Getenv(telemetryPTYMode)
	if mode == "" {
		return
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		t.Fatal("the child needs two real terminal descriptors")
	}
	root := os.Getenv("CROSSHALL_STATE_DIR")
	ctx := t.Context()
	env := cli.Env{
		Stdin:              os.Stdin,
		SetupTerminal:      func() bool { return isTerminal(os.Stdin) && isTerminal(os.Stdout) },
		StartReady:         func(context.Context) error { return nil },
		TelemetryEndpoint:  "https://example.invalid",
		TelemetryStateRoot: root,
		ResolveTelemetryPolicyPath: func(path string) string {
			if path != "/etc/crosshall/telemetry-off" {
				t.Fatalf("default telemetry policy path = %q", path)
			}
			return filepath.Join(root, "telemetry-off")
		},
		Setup: func(req cli.SetupRequest) error {
			svc := setup.Service{StateRoot: root, Executable: os.Args[0]}
			return svc.Run(ctx, setup.Request(req))
		},
	}
	args := []string{"owner", "start"}
	if mode == "setup" {
		args = []string{"setup", "--offer"}
	}
	if code := cli.RunEnv(ctx, env, args, os.Stdout, os.Stderr); code != core.ExitOK {
		t.Fatalf("exit %d", code)
	}
}

func TestTelemetryConsentWithAPseudoTerminal(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	harness, err := filepath.Abs("../../test/setupfixture/terminal.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, command, input, ci, wantMode, doNotTrack string
		inCI, wantQuestion                             bool
	}{
		{name: "owner CI", command: "owner", input: "\n", ci: "1", inCI: true, wantMode: telemetryconfig.Unset},
		{name: "setup CI with empty marker", command: "setup", input: "\n", inCI: true, wantMode: telemetryconfig.Unset},
		{name: "whitespace veto", command: "setup", input: "\n", doNotTrack: " ", wantMode: telemetryconfig.Unset},
		{name: "Enter", command: "owner", input: "\n", wantQuestion: true, wantMode: telemetryconfig.On},
		{name: "partial EOF", command: "setup", input: " \x04", wantQuestion: true, wantMode: telemetryconfig.Unset},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := setuptest.New(t)
			t.Setenv(telemetryPTYMode, tt.command)
			t.Setenv(telemetryconfig.TelemetryEnv, "")
			t.Setenv(telemetryconfig.DoNotTrackEnv, tt.doNotTrack)
			t.Setenv("CI", tt.ci)
			if !tt.inCI {
				if err := os.Unsetenv("CI"); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), python, "-B", harness, "--answers", tt.input, "--",
				os.Args[0], "-test.run=^TestTelemetryTerminalChild$")
			cmd.Dir = fixture.Home
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("terminal child: %v\n%s", err, out)
			}
			if question := strings.Contains(string(out), "Send usage statistics? [Y/n]"); question != tt.wantQuestion {
				t.Fatalf("unexpected question state: %s", out)
			}
			got, err := telemetryconfig.ReadMode(fixture.State)
			if err != nil || got.Mode != tt.wantMode {
				t.Fatalf("mode=%q want %q, error=%v", got.Mode, tt.wantMode, err)
			}
		})
	}
}
