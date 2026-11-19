package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// setupEnv records the request each setup command reaches the service with.
func setupEnv(terminal bool, got *[]SetupRequest) Env {
	return Env{
		OwnerGate:     func() string { return "" },
		SetupTerminal: func() bool { return terminal },
		SetupAdapters: []string{"claude", "codex"},
		Setup: func(req SetupRequest) error {
			*got = append(*got, req)
			return nil
		},
	}
}

func TestSetupFormsReachTheServiceAsParsed(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		terminal bool
		want     SetupRequest
	}{
		{[]string{"setup"}, true, SetupRequest{Action: "setup", Terminal: true}},
		{[]string{"setup", "codex"}, true, SetupRequest{Action: "setup", Adapters: []string{"codex"}, Terminal: true}},
		{[]string{"setup", "--dry-run"}, false, SetupRequest{Action: "setup", DryRun: true}},
		{[]string{"setup", "--dry-run", "claude", "codex"}, false, SetupRequest{Action: "setup", DryRun: true, Adapters: []string{"claude", "codex"}}},
		{[]string{"setup", "claude", "codex", "--apply"}, false, SetupRequest{Action: "setup", Apply: true, Adapters: []string{"claude", "codex"}}},
		{[]string{"setup", "claude"}, false, SetupRequest{Action: "setup", Adapters: []string{"claude"}}},
		{[]string{"setup", "status"}, false, SetupRequest{Action: "status"}},
		{[]string{"setup", "off"}, false, SetupRequest{Action: "off"}},
		{[]string{"setup", "--offer"}, true, SetupRequest{Action: "setup", Offer: true, Terminal: true}},
		{[]string{"uninstall"}, false, SetupRequest{Action: "uninstall"}},
		{[]string{"uninstall", "codex", "--dry-run"}, false, SetupRequest{Action: "uninstall", DryRun: true, Adapters: []string{"codex"}}},
	} {
		var got []SetupRequest
		_, stderr, code := run(t, setupEnv(tc.terminal, &got), tc.args...)
		if code != core.ExitOK || len(got) != 1 {
			t.Errorf("%v: exit %d, %d calls, %s", tc.args, code, len(got), stderr)
			continue
		}
		r := got[0]
		if r.Action != tc.want.Action || r.DryRun != tc.want.DryRun || r.Apply != tc.want.Apply || r.Offer != tc.want.Offer ||
			r.Terminal != tc.want.Terminal || !slices.Equal(r.Adapters, tc.want.Adapters) {
			t.Errorf("%v: got %+v", tc.args, r)
		}
	}
}

func TestSetupRejectsMalformedForms(t *testing.T) {
	for _, args := range [][]string{
		{"setup", "gemini"},
		{"setup", "claude", "claude"},
		{"setup", "--apply"},
		{"setup", "--dry-run", "claude", "--apply"},
		{"setup", "off", "claude"},
		{"setup", "status", "claude"},
		{"setup", "--offer", "claude"},
		{"setup", "--offer", "--apply"},
		{"uninstall", "--apply", "claude"},
		{"uninstall", "off"},
		{"setup", "claude", "--bogus"},
	} {
		var got []SetupRequest
		_, _, code := run(t, setupEnv(true, &got), args...)
		if code != core.ExitUsage || len(got) != 0 {
			t.Errorf("%v: exit %d, %d calls", args, code, len(got))
		}
	}
}

// An unnamed setup without a terminal used to read piped answers (rc.6).
// It now explains the scripted form and stops before the service runs.
func TestUnnamedSetupWithoutATerminalPrintsTheScriptedForm(t *testing.T) {
	var got []SetupRequest
	_, stderr, code := run(t, setupEnv(false, &got), "setup")
	if code != core.ExitUsage || len(got) != 0 {
		t.Fatalf("exit %d, %d calls", code, len(got))
	}
	for _, want := range []string{"crosshall setup claude codex --apply", "crosshall setup --dry-run claude codex", "no longer read"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// The owner gate runs before parsing reaches the service, for every form,
// the private installer form included.
func TestEverySetupFormIsOwnerOnly(t *testing.T) {
	for _, args := range [][]string{{"setup"}, {"setup", "status"}, {"setup", "--offer"}, {"setup", "claude", "--apply"}, {"uninstall", "codex"}} {
		var got []SetupRequest
		env := setupEnv(true, &got)
		env.OwnerGate = func() string { return "CLAUDE_CODE_SESSION_ID is set" }
		out, stderr, code := run(t, env, args...)
		offer := len(args) == 2 && args[1] == "--offer"
		if len(got) != 0 || (code == core.ExitOK) != offer || offer && out+stderr != "" {
			t.Errorf("%v: exit %d, %d calls, %q", args, code, len(got), out+stderr)
		}
	}
}

type reported struct{}

func (reported) Error() string  { return "setup did not finish for codex" }
func (reported) Reported() bool { return true }

// A failure the JSON result already carries is not written a second time.
func TestAReportedSetupFailureWritesOneJSONObject(t *testing.T) {
	env := setupEnv(true, new([]SetupRequest))
	env.Setup = func(req SetupRequest) error {
		req.Output.Write([]byte(`{"v":1,"ok":false}` + "\n"))
		return reported{}
	}
	out, stderr, code := run(t, env, "--json", "setup", "codex", "--apply")
	if code != core.ExitError || out != `{"v":1,"ok":false}`+"\n" || stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, stderr)
	}
}

// The installer form is silent when the setup service is
// unavailable (for example an invalid state directory) or there is no
// terminal, and the service is not reached.
func TestTheOfferIsSilentWithoutAServiceOrATerminal(t *testing.T) {
	var got []SetupRequest
	env := setupEnv(false, &got)
	if out, stderr, code := run(t, env, "setup", "--offer"); code != core.ExitOK || out+stderr != "" || len(got) != 0 {
		t.Fatalf("no terminal: exit %d %q %d calls", code, out+stderr, len(got))
	}
	env = setupEnv(true, &got)
	env.Setup, env.Unavailable = nil, "CROSSHALL_STATE_DIR must be an absolute path"
	if out, stderr, code := run(t, env, "setup", "--offer"); code != core.ExitOK || out+stderr != "" {
		t.Fatalf("unavailable: exit %d %q", code, out+stderr)
	}
}
