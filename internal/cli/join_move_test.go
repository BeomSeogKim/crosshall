package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestJoinParsesMoveAndPrintsTheSourceCounts(t *testing.T) {
	move := &domain.JoinMoveReceipt{Channel: "checkout", Unread: 2, Owed: 1, Waiting: 3}
	ft := &fakeTransport{result: okResult(t, domain.JoinResult{
		Channel: "inventory", Alias: "writer", Hint: domain.Hint("writer", "inventory"), Move: move,
	})}
	stdout, stderr, code := run(t, Env{Transport: ft}, "--key", "move-1", "join", "#inventory", "--move", "--as", "@writer")
	if code != core.ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	want := domain.Join{Channel: "inventory", Alias: "writer", Move: true}
	if len(ft.calls) != 1 || ft.calls[0].Command != want || ft.calls[0].OpKey != "move-1" || ft.calls[0].Owner {
		t.Fatalf("wrong call: %+v", ft.calls)
	}
	for _, line := range []string{
		"Work stays in #checkout (2 unread, 1 owed, 3 waiting).",
		"Joined #inventory as @writer",
	} {
		if !strings.Contains(stdout, line) {
			t.Fatalf("missing %q in %s", line, stdout)
		}
	}
}

func TestJoinMoveRequiresAnExplicitDestination(t *testing.T) {
	ft := &fakeTransport{}
	_, stderr, code := run(t, Env{Transport: ft}, "join", "--move", "--as", "writer")
	if code != core.ExitUsage || len(ft.calls) != 0 || !strings.Contains(stderr, "crosshall join <channel> --move") {
		t.Fatalf("exit %d, calls %+v, stderr %q", code, ft.calls, stderr)
	}
}

func TestJoinMoveJSONKeepsDestinationAndSourceSeparate(t *testing.T) {
	move := domain.JoinMoveReceipt{Channel: "checkout", Owed: 1}
	ft := &fakeTransport{result: okResult(t, domain.JoinResult{Channel: "inventory", Alias: "writer", Move: &move})}
	stdout, stderr, code := run(t, Env{Transport: ft}, "--json", "join", "inventory", "--move")
	if code != core.ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var got struct {
		OK      bool                   `json:"ok"`
		Channel string                 `json:"channel"`
		Alias   string                 `json:"alias"`
		Move    domain.JoinMoveReceipt `json:"move"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Channel != "inventory" || got.Alias != "writer" || got.Move != move {
		t.Fatalf("unexpected result: %s", stdout)
	}
}
