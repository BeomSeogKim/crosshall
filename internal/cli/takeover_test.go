package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestTakeoverCommandsParseAndRender(t *testing.T) {
	at := time.Date(2026, 11, 25, 16, 39, 0, 0, time.Local).UnixMilli()
	lines := takeoverLines([]domain.OwnerTakeoverLine{
		{ID: 2481, BindingID: 37, Channel: "dev", Alias: "vega", Vendor: "codex", Session: "3f2a9c1b0d4e",
			EndedAtMS: at, LastSeenMS: at - 60_000, Owes: []int64{7}, AskedAtMS: at + 6*60_000},
		{ID: 2490, BindingID: 38, Channel: "dev", Alias: "sol", Vendor: "claude", Session: "0123456789ab",
			EndedAtMS: at, Verified: true, AskedAtMS: at, Granted: true, ExpiresAtMS: at + time.Hour.Milliseconds()},
	})
	want0 := "takeover 2481: #dev/@vega (binding 37), session ended " + clock(at) + " (unverified), last seen " + clock(at-60_000) +
		"; owes #7. Asked by codex session sha256:3f2a9c1b0d4e at " + clock(at+6*60_000) + ". crosshall owner takeover 2481"
	if len(lines) != 2 || lines[0] != want0 || !strings.Contains(lines[1], "Granted; it completes when that session runs crosshall join dev --as sol --takeover again") {
		t.Fatalf("owner who lines %q", lines)
	}

	ft := &fakeTransport{result: okResult(t, domain.OwnerTakeoverResult{ID: 2481, BindingID: 37, Channel: "dev", Alias: "vega", Session: "3f2a9c1b0d4e", Completed: true})}
	stdout, _, code := run(t, Env{Transport: ft}, "--key", "k", "owner", "takeover", "2481")
	if code != core.ExitOK || stdout != "@vega in #dev is now session sha256:3f2a9c1b0d4e (takeover 2481, binding 37).\n" {
		t.Fatalf("owner takeover: %d %q", code, stdout)
	}
	if cmd, ok := ft.calls[0].Command.(domain.OwnerTakeover); !ok || cmd.ID != 2481 || ft.calls[0].OpKey == "" {
		t.Fatalf("sent %+v", ft.calls[0])
	}
	for _, args := range [][]string{{"owner", "takeover"}, {"owner", "takeover", "0"}, {"owner", "takeover", "1", "2"}} {
		if _, _, code := run(t, Env{Transport: &fakeTransport{}}, args...); code != core.ExitUsage {
			t.Errorf("%q exited %d, want usage", args, code)
		}
	}

	ft = &fakeTransport{result: okResult(t, domain.JoinResult{Channel: "dev", Alias: "vega",
		Takeover: &domain.TakeoverRequest{ID: 2481, Channel: "dev", Alias: "vega", Session: "3f2a9c1b0d4e"}})}
	stdout, _, _ = run(t, Env{Transport: ft}, "join", "dev", "--as", "vega", "--takeover")
	if !strings.Contains(stdout, "Asked the owner to give @vega in #dev to this session, sha256:3f2a9c1b0d4e (takeover 2481).") ||
		!strings.Contains(stdout, "run crosshall join dev --as vega --takeover again within an hour") {
		t.Fatalf("join --takeover printed %q", stdout)
	}
	if cmd, ok := ft.calls[0].Command.(domain.Join); !ok || !cmd.Takeover || cmd.Alias != "vega" || cmd.Channel != "dev" {
		t.Fatalf("sent %+v", ft.calls[0].Command)
	}
	if _, _, code := run(t, Env{Transport: &fakeTransport{}}, "join", "dev", "--takeover"); code != core.ExitUsage {
		t.Fatalf("join --takeover without an alias exited %d", code)
	}

	// An older dispatcher refuses the field it does not know.
	ft = &fakeTransport{result: domain.Fail(core.ExitUsage, "", "invalid command arguments", "")}
	_, stderr, _ := run(t, Env{Transport: ft}, "join", "dev", "--as", "vega", "--takeover")
	if !strings.Contains(stderr, "this dispatcher does not take takeover requests yet") || !strings.Contains(stderr, "owner restore") {
		t.Fatalf("older dispatcher refusal %q", stderr)
	}
}
