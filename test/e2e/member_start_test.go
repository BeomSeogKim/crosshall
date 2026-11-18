//go:build e2e

package e2e

import (
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestMemberRestartsExistingDispatcher(t *testing.T) {
	w, _, members := setup(t)
	w.kill()

	if got := sortedAliases(members[0].must("who")); !reflect.DeepEqual(got, three) {
		t.Fatalf("roster after member restart: %v", got)
	}
	if got := w.dispatcherPIDs(); len(got) != 1 {
		t.Fatalf("member command left %d dispatchers: %v", len(got), got)
	}
}

func TestMemberCannotInitializeState(t *testing.T) {
	w := newWorld(t)
	out := w.member().run("join", "checkout")
	if out.exit != 5 || out.obj["code"] != "unsent" {
		t.Fatalf("join without owner setup: exit %d, output %s", out.exit, out.stdout)
	}
	if _, err := os.Stat(w.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("member initialized the state root: %v", err)
	}
}
