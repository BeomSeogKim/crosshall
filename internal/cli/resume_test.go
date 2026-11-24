package cli

import (
	"context"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestOwedLineAfterOwnerStart(t *testing.T) {
	if got := owedLine(nil); got != "" {
		t.Fatalf("no owing member printed %q", got)
	}
	members := []domain.OwingMember{
		{BindingID: 1, Channel: "dev", Alias: "lyra", Owed: 2, Wake: domain.Wake{Wake: domain.WakeNotice}},
		{BindingID: 2, Channel: "dev", Alias: "sol", Owed: 1, Wake: domain.Wake{Wake: domain.WakeNone, Why: domain.WhyNoRoute, Adapter: "codex"}},
		{BindingID: 3, Channel: "dev", Alias: "vega", Owed: 1, Wake: domain.Wake{Wake: domain.WakeNone, Why: domain.WhyUnproven}},
	}
	want := "3 members owe 4 open requests: 2 can be woken and get one resume notice when their turn is free; " +
		"1 cannot be woken (@sol in #dev: no notice route for codex)."
	if got := owedLine(members); got != want {
		t.Fatalf("owed line\n got %q\nwant %q", got, want)
	}
	if got := owedLine(members[:1]); got != "1 member owes 2 open requests: 1 can be woken and get one resume notice when their turn is free." {
		t.Fatalf("one member: %q", got)
	}
}

// owner start asks the started dispatcher who owes; an older dispatcher's
// refusal of the query prints nothing more.
func TestOwnerStartPrintsWhoOwes(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.OwnerOwedResult{Members: []domain.OwingMember{
		{BindingID: 1, Channel: "dev", Alias: "lyra", Owed: 1, Wake: domain.Wake{Wake: domain.WakeNotice}}}})}
	env := Env{Start: func(context.Context) error { return nil }, Transport: ft}
	stdout, _, _ := run(t, env, "owner", "start")
	if want := "dispatcher running\n1 member owes 1 open request: 1 can be woken and get one resume notice when their turn is free.\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	if _, ok := ft.calls[0].Command.(domain.OwnerOwed); !ok || !ft.calls[0].Owner {
		t.Fatalf("sent %+v", ft.calls[0])
	}
	env.Transport = &fakeTransport{result: domain.Fail(core.ExitUsage, "", "invalid command arguments", "")}
	if stdout, _, _ = run(t, env, "owner", "start"); stdout != "dispatcher running\n" {
		t.Fatalf("older dispatcher: %q", stdout)
	}
}
