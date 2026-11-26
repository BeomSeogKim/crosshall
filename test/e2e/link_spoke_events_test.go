//go:build e2e

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// The spoke keeps the home's event stream on a connection of its own: a
// member's scope reaches the spoke store and the home installs the
// spoke's cut, a reconnect resumes from the cursor, a restart asks for a
// snapshot, and member commands keep working beside it (#400, 12j).
func TestSpokeLinkEventStream(t *testing.T) {
	f := newLinkFixture(t, binary, binary)
	home, spoke := f.home.owner(), f.spoke.owner()
	home.must("owner", "start")
	spoke.must("owner", "start")
	home.must("owner", "channel", "create", "home-only")
	spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
	waitSpokeLink(t, spoke, "connected")
	member := f.spoke.member()
	member.must("join", "home-only")

	homeDB := f.home.db()
	spokeDB := spokeStore(t, f.spoke.state)
	eventually(t, "the spoke applied the member's scope", func() bool {
		var binding, eligible int64
		err := spokeDB.QueryRow(`SELECT s.binding_id, c.eligible FROM spoke_member_scope s, spoke_event_cursor c`).Scan(&binding, &eligible)
		return err == nil && binding > 0 && eligible == 1
	})
	eventually(t, "the home installed the spoke's cut", func() bool {
		var cuts, acked int64
		err := homeDB.QueryRow(`SELECT (SELECT COUNT(*) FROM link_event_cut WHERE absent = 0), acked FROM link_event_cursor`).Scan(&cuts, &acked)
		return err == nil && cuts > 0 && acked > 0
	})
	subscribed := regexp.MustCompile(`event stream subscribed from (\d+) \(snapshot (true|false)\)`)
	subscriptions := func() [][]string {
		data, _ := os.ReadFile(filepath.Join(f.spoke.state, "dispatcher.log"))
		return subscribed.FindAllStringSubmatch(string(data), -1)
	}
	if got := subscriptions(); len(got) != 1 || got[0][2] != "true" {
		t.Fatalf("first subscription %v", got)
	}

	// A killed connection resumes from the cursor without a snapshot.
	killSpokeSSH(t, f.ssh)
	waitSpokeLink(t, spoke, "connected")
	eventually(t, "the stream resumed", func() bool { return len(subscriptions()) == 2 })
	if got := subscriptions()[1]; got[1] == "0" || got[2] != "false" {
		t.Fatalf("resumed subscription %v", got)
	}
	member.must("who")

	// A new dispatcher process asks for a snapshot.
	running := linkDispatcher(f.spoke)
	if err := running.process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the spoke dispatcher stopped", func() bool { return !running.alive() })
	// A member command starts the dispatcher again; it may find the link
	// still connecting (unsent).
	member.run("who")
	waitSpokeLink(t, spoke, "connected")
	eventually(t, "the restarted stream subscribed", func() bool { return len(subscriptions()) == 3 })
	if got := subscriptions()[2]; got[2] != "true" {
		t.Fatalf("restarted subscription %v", got)
	}
	eventually(t, "the snapshot applied again", func() bool {
		var eligible int64
		return spokeDB.QueryRow(`SELECT eligible FROM spoke_event_cursor`).Scan(&eligible) == nil && eligible == 1
	})
	member.must("who")

	live := liveSpokeSSH(t, f.ssh)
	spoke.must("unlink")
	waitSpokeLink(t, spoke, "unlinked")
	for pid, start := range live {
		if proc.ProcessAlive(int64(pid), start.Identity) {
			t.Fatal("unlink left an SSH child active")
		}
	}
}

func spokeStore(t *testing.T, state string) *sql.DB {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(state, "spoke", "*", "store.sqlite3"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("spoke stores %v, %v", paths, err)
	}
	db, err := sql.Open("sqlite", "file:"+paths[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(fixtureEventTimeout); !ok(); time.Sleep(30 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting until %s", what)
		}
	}
}
