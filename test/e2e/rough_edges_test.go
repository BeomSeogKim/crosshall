//go:build e2e

package e2e

import (
	"os/exec"
	"strconv"
	"testing"
)

func TestVersionFlagEndToEnd(t *testing.T) {
	version, err := exec.Command(binary, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	flag, err := exec.Command(binary, "--version").Output()
	if err != nil || string(flag) != string(version) || len(version) < 2 {
		t.Fatalf("--version printed %q (%v), version printed %q", flag, err, version)
	}
}

func TestOwnerDoctorNamesTheStartCommandEndToEnd(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	if fresh := owner.run("owner", "doctor"); fresh.exit != 1 || fresh.obj["hint"] != "crosshall owner start" {
		t.Fatalf("never started: exit %d %s", fresh.exit, fresh.stdout)
	}
	owner.must("owner", "start")
	if healthy := owner.must("owner", "doctor"); healthy.obj["hint"] != nil {
		t.Fatalf("running dispatcher still gets a hint: %s", healthy.stdout)
	}
	w.kill()
	if stopped := owner.run("owner", "doctor"); stopped.exit != 1 || stopped.obj["hint"] != "crosshall owner start" {
		t.Fatalf("stopped: exit %d %s", stopped.exit, stopped.stdout)
	}
}

func TestOwnerLogPagesEndToEnd(t *testing.T) {
	_, owner, members := setup(t)
	form := members[0]
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, num(t, form.must("note", "@tax", "note "+strconv.Itoa(i)), "id"))
	}
	latest := owner.must("owner", "log", "checkout", "--limit", "2")
	if got := entryIDs(latest); len(got) != 2 || got[0] != ids[3] || got[1] != ids[4] || num(t, latest, "earlier") != 3 || num(t, latest, "next_since") != 0 {
		t.Fatalf("latest page %v: %s", got, latest.stdout)
	}
	first := owner.must("owner", "log", "#checkout", "--since", "0", "--limit", "2")
	if got := entryIDs(first); len(got) != 2 || got[0] != ids[0] || num(t, first, "later") != 3 {
		t.Fatalf("first page %v: %s", got, first.stdout)
	}
	if bad := owner.run("owner", "log", "checkout", "--limit", "0"); bad.exit != 2 {
		t.Fatalf("--limit 0: exit %d %s", bad.exit, bad.stdout)
	}
}

func TestMemberStatusShowsItsOwnRoundsEndToEnd(t *testing.T) {
	_, _, members := setup(t)
	form, tax, lint := members[0], members[1], members[2]
	ask := form.must("ask", "@tax", "check the rounding")
	exchange := num(t, ask, "exchange")
	for _, tc := range []struct {
		p         *participant
		round     float64
		exchanges int
	}{{form, 0, 0}, {tax, 1, 1}, {lint, 0, 0}} {
		self, _ := tc.p.must("status").obj["self"].(map[string]any)
		list, _ := self["exchanges"].([]any)
		if self == nil || self["round"] != tc.round || self["round_limit"] != float64(8) || len(list) != tc.exchanges {
			t.Fatalf("unexpected member rounds: %v", self)
		}
		if tc.exchanges == 1 {
			x := list[0].(map[string]any)
			if x["id"] != float64(exchange) || x["round"] != float64(1) {
				t.Fatalf("exchange %v, want x%d at round 1", x, exchange)
			}
		}
	}
}

func entryIDs(o outcome) []int64 {
	var out []int64
	messages, _ := o.obj["messages"].([]any)
	for _, raw := range messages {
		if m, ok := raw.(map[string]any); ok {
			if id, ok := m["id"].(float64); ok {
				out = append(out, int64(id))
			}
		}
	}
	return out
}
