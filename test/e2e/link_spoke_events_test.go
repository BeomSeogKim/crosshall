//go:build e2e

package e2e

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// A home restored from an older backup stops its linked device (v1): the
// device applies nothing of the older stream, its commands refuse with
// the way back, and linking again starts a fresh spoke store, the old one
// kept aside.
func TestSpokeLinkHomeRestoredFromBackup(t *testing.T) {
	t.Run("link again", func(t *testing.T) { homeRestoredFromBackup(t, false) })
	t.Run("unlink and enroll again", func(t *testing.T) { homeRestoredFromBackup(t, true) })
}

func homeRestoredFromBackup(t *testing.T, unlink bool) {
	f := newLinkFixture(t, binary, binary)
	home, spoke := f.home.owner(), f.spoke.owner()
	home.must("owner", "start")
	spoke.must("owner", "start")
	home.must("owner", "channel", "create", "first")
	home.must("owner", "channel", "create", "second")
	spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
	waitSpokeLink(t, spoke, "connected")
	member := f.spoke.member()
	member.must("join", "first")
	spokeDB := spokeStore(t, f.spoke.state)
	acked := func() int64 {
		var seq, eligible int64
		if spokeDB.QueryRow(`SELECT acked_seq, eligible FROM spoke_event_cursor`).Scan(&seq, &eligible) != nil || eligible == 0 {
			return 0
		}
		return seq
	}
	eventually(t, "the spoke acknowledged the join", func() bool { return acked() > 0 })
	// Moved before the backup: the restored home selects #second at a
	// generation above 1, which a stampless call does not match.
	if got := member.run("join", "second", "--move"); got.exit != 0 {
		t.Fatalf("join second: %+v", got)
	}
	moved := acked()
	eventually(t, "the spoke acknowledged the move", func() bool { return acked() > moved })

	backup := t.TempDir()
	home.must("owner", "stop")
	copyStore(t, f.home.state, backup)
	home.must("owner", "start")
	waitSpokeLink(t, spoke, "connected")
	before := acked()
	if got := member.run("join", "first", "--move"); got.exit != 0 {
		t.Fatalf("join first after the backup: %+v", got)
	}
	eventually(t, "the spoke acknowledged events after the backup", func() bool { return acked() > before })

	home.must("owner", "stop")
	copyStore(t, backup, f.home.state)
	home.must("owner", "start")
	eventually(t, "the spoke saw the home go back", func() bool {
		var stopped int64
		return spokeDB.QueryRow(`SELECT rolled_back FROM spoke_event_cursor`).Scan(&stopped) == nil && stopped == 1
	})
	got := member.run("who")
	if got.obj["code"] != "home_rolled_back" || !strings.Contains(got.stdout, "crosshall link "+linkFixtureTarget+" again") {
		t.Fatalf("a command after the home went back: %+v", got)
	}
	// A new dispatcher process reads the stop from the store.
	running := linkDispatcher(f.spoke)
	if err := running.process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the spoke dispatcher stopped", func() bool { return !running.alive() })
	if got := member.run("who"); got.obj["code"] != "home_rolled_back" {
		t.Fatalf("a command after a restart: %+v", got)
	}

	if unlink {
		spoke.must("unlink")
		spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
	} else {
		spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
	}
	waitSpokeLink(t, spoke, "connected")
	aside, _ := filepath.Glob(filepath.Join(f.spoke.state, "spoke", "*-rolled-back-*", "store.sqlite3"))
	if len(aside) != 1 {
		t.Fatalf("the old spoke store was not kept aside: %v", aside)
	}
	if got := member.run("join", "second"); got.exit != 0 {
		data, _ := os.ReadFile(filepath.Join(f.spoke.state, "dispatcher.log"))
		if len(data) > 3000 {
			data = data[len(data)-3000:]
		}
		t.Fatalf("join after linking again: %+v\n%s", got, data)
	}
	if got := member.must("who"); !strings.Contains(got.stdout, "second") {
		t.Fatalf("who after linking again: %+v", got)
	}
}

// copyStore copies a stopped hall's store files from one state directory
// to another, replacing what is there.
func copyStore(t *testing.T, from, to string) {
	t.Helper()
	for _, name := range []string{"store.sqlite3", "store.sqlite3-wal", "store.sqlite3-shm"} {
		data, err := os.ReadFile(filepath.Join(from, name))
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Remove(filepath.Join(to, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
