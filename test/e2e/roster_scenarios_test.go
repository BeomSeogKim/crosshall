//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestRosterCapAndAcknowledgment(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	for i := 1; i <= 14; i++ {
		alias := fmt.Sprintf("reviewer-%02d", i)
		w.member().must("join", "checkout", "--as", alias, "--focus", "review")
	}

	observer := w.member()
	client := spool.Client{StateRoot: w.state, Rendezvous: w.rendezvous()}
	id := ident.Identity{Vendor: "route", Key: observer.route}
	// A successful client response can wait for rendering. The CLI processes
	// its own next response normally, including the acknowledgment.
	beforeRendering := func(verb, args string) []byte {
		t.Helper()
		r, err := client.Member(t.Context(), id, wire.Request{Verb: verb, Args: json.RawMessage(args)})
		if err != nil || r.Status != core.ExitOK {
			t.Fatalf("%s before rendering: exit %d, %s, error %v", verb, r.Status, r.Data, err)
		}
		return r.Data
	}
	roster := func(data []byte) domain.JoinResult {
		t.Helper()
		var r domain.JoinResult
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := roster(beforeRendering("join", `{"channel":"checkout","alias":"observer","focus":"review"}`))
	repeated := roster([]byte(observer.must("join", "checkout").stdout))
	if first.Restored || !repeated.Restored || first.Hint != repeated.Hint {
		t.Fatalf("expected initial and restored snapshots: first=%+v repeated=%+v", first, repeated)
	}
	for _, r := range []domain.JoinResult{first, repeated} {
		if len(r.Members) != 12 || r.Omitted != 3 || !strings.Contains(r.Hint, "crosshall who") {
			t.Fatalf("expected 12 members, 3 omitted and the roster command: %+v", r)
		}
		if !r.Members[0].Self || r.Members[0].Alias != "observer" {
			t.Fatalf("joiner's row missing from capped roster: %+v", r.Members[0])
		}
		seen := make(map[string]bool)
		for _, m := range r.Members {
			if m.Alias == "" || seen[m.Alias] || m.Focus != "review" || m.State != "unknown" || m.Route != "pull" {
				t.Fatalf("invalid or repeated shell member in roster: %+v", m)
			}
			seen[m.Alias] = true
		}
	}
	resumed := roster([]byte(observer.must("join", "checkout").stdout))
	if !resumed.Restored || !strings.HasPrefix(resumed.Hint, "[crosshall] Already in #checkout as @observer.") ||
		len(resumed.Members) != 0 || len(resumed.Changes) != 0 {
		t.Fatalf("resumed member repeated acknowledged roster: %+v", resumed)
	}

	w.member().must("join", "checkout", "--as", "late", "--focus", "validation")
	beforeAck := roster(beforeRendering("inbox", "{}")).Changes
	atRender := roster([]byte(observer.must("inbox").stdout)).Changes
	for _, changes := range [][]domain.Member{beforeAck, atRender} {
		if len(changes) != 1 || changes[0].Alias != "late" || changes[0].Change != "joined" || changes[0].Focus != "validation" {
			t.Fatalf("expected only the new member: %+v", changes)
		}
	}
	if changes := roster([]byte(observer.must("inbox").stdout)).Changes; len(changes) != 0 {
		t.Fatalf("acknowledged join repeated: %+v", changes)
	}
	if got := len(observer.must("who").aliases()); got != 16 {
		t.Fatalf("full roster has %d members, want 16", got)
	}
}
