package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// setupEnv records the request each setup command reaches the service with.
func setupEnv(terminal bool, got *[]SetupRequest) Env {
	return Env{
		OwnerGate:     func() string { return "" },
		SetupTerminal: func() bool { return terminal },
		SetupAdapters: []string{"claude", "codex"},
		Setup: func(_ context.Context, req SetupRequest) error {
			*got = append(*got, req)
			return nil
		},
	}
}

func TestOnlyAnApplyUsesItsOperationDeadline(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"setup", "codex", "--apply"}, true},
		{[]string{"setup", "codex"}, false},
		{[]string{"setup", "codex", "--dry-run"}, false},
		{[]string{"uninstall", "codex", "--apply"}, true},
		{[]string{"uninstall", "--apply"}, true},
		{[]string{"uninstall", "codex"}, false},
		{[]string{"uninstall", "--dry-run"}, false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			env := setupEnv(true, new([]SetupRequest))
			env.CommandDeadline = time.Now().Add(-time.Second)
			deadline := time.Now().Add(time.Minute)
			var started, stopped bool
			env.BeginSetupApply = func(ctx context.Context, action string) (context.Context, func()) {
				started = true
				if action != tc.args[0] {
					t.Errorf("deadline for %q, want %q", action, tc.args[0])
				}
				ctx, cancel := context.WithDeadline(ctx, deadline)
				return ctx, func() { stopped = true; cancel() }
			}
			env.Setup = func(ctx context.Context, _ SetupRequest) error {
				got, has := ctx.Deadline()
				if has != tc.want || has && !got.Equal(deadline) {
					t.Errorf("setup service deadline = %v, %v; want dedicated deadline = %v", got, has, tc.want)
				}
				return nil
			}
			if out, stderr, code := run(t, env, tc.args...); code != core.ExitOK || started != tc.want || stopped != tc.want {
				t.Fatalf("exit %d, started %v, stopped %v, stdout %q, stderr %q", code, started, stopped, out, stderr)
			}
		})
	}
}

func TestAnApplyPastItsDeadlineKeepsARealError(t *testing.T) {
	timeout := errors.New("uninstall timed out")
	for _, tc := range []struct {
		name      string
		returned  error
		want, not string
	}{
		{"real error", errors.New("configuration changed"), "configuration changed. uninstall timed out", ""},
		{"wrapped context error", fmt.Errorf("codex adapter policy: %w", context.Canceled), "codex adapter policy: context canceled. uninstall timed out", ""},
		{"bare context error", context.Canceled, "uninstall timed out", "context canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupEnv(true, new([]SetupRequest))
			var expire context.CancelCauseFunc
			env.BeginSetupApply = func(ctx context.Context, _ string) (context.Context, func()) {
				ctx, expire = context.WithCancelCause(ctx)
				return ctx, func() { expire(nil) }
			}
			env.Setup = func(context.Context, SetupRequest) error {
				expire(timeout)
				return tc.returned
			}
			if _, stderr, code := run(t, env, "uninstall", "codex", "--apply"); code != core.ExitError || !strings.Contains(stderr, tc.want) || tc.not != "" && strings.Contains(stderr, tc.not) {
				t.Fatalf("exit %d, stderr %q, want %q", code, stderr, tc.want)
			}
		})
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
		{[]string{"uninstall", "--apply"}, false, SetupRequest{Action: "uninstall", Apply: true}},
		{[]string{"uninstall", "--apply", "claude"}, false, SetupRequest{Action: "uninstall", Apply: true, Adapters: []string{"claude"}}},
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
		{"uninstall", "--apply", "claude", "--dry-run"},
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
	env.Setup = func(_ context.Context, req SetupRequest) error {
		req.Output.Write([]byte(`{"v":1,"ok":false}` + "\n"))
		return reported{}
	}
	out, stderr, code := run(t, env, "--json", "setup", "codex", "--apply")
	if code != core.ExitError || out != `{"v":1,"ok":false}`+"\n" || stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, stderr)
	}
}

func TestSetupApplyDeadlineRendersConfirmedPolicyAndRetry(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			deadline := errors.New("setup apply timed out. Run setup status, then retry with the same adapters")
			env := setupEnv(false, new([]SetupRequest))
			var expire context.CancelCauseFunc
			env.BeginSetupApply = func(ctx context.Context, _ string) (context.Context, func()) {
				ctx, expire = context.WithCancelCause(ctx)
				return ctx, func() { expire(nil) }
			}
			env.Setup = func(context.Context, SetupRequest) error {
				expire(deadline)
				return errors.New("adapter policy was applied. Dispatcher did not confirm automatic joining (outcome_unknown)")
			}
			args := []string{"setup", "codex", "--apply"}
			if format == "json" {
				args = append([]string{"--json"}, args...)
			}
			out, stderr, code := run(t, env, args...)
			text := out + stderr
			if format == "json" {
				var result struct{ Message string }
				if err := json.Unmarshal([]byte(out), &result); err != nil {
					t.Fatalf("setup result %q: %v", out, err)
				}
				text = result.Message
			}
			if code != core.ExitError || !strings.Contains(text, "adapter policy was applied") ||
				!strings.Contains(text, "automatic joining (outcome_unknown)") || !strings.Contains(text, deadline.Error()) {
				t.Fatalf("exit %d, rendered setup error %q", code, text)
			}
		})
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
