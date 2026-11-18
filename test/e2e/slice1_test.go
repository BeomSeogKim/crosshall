//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// setup starts a fresh installation with channel checkout and three members.
func setup(t *testing.T) (*world, *participant, []*participant) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout", "--topic", "payment screen")
	var members []*participant
	for _, alias := range []string{"form", "tax", "lint"} {
		m := w.member()
		m.must("join", "checkout", "--as", alias)
		members = append(members, m)
	}
	t.Cleanup(w.checkShellDelivery)
	return w, owner, members
}

func sortedAliases(o outcome) []string {
	a := o.aliases()
	slices.Sort(a)
	return a
}

var three = []string{"form", "lint", "tax"}

// From an empty home: start, create a channel, join from three separate shell
// identities, and each who lists all three.
func TestFreshInstallThreeMembers(t *testing.T) {
	w, _, members := setup(t)
	if _, err := os.Stat(w.state); err != nil {
		t.Fatalf("state root not created: %v", err)
	}
	for i, m := range members {
		got := m.must("who")
		if !reflect.DeepEqual(sortedAliases(got), three) {
			t.Fatalf("member %d sees %v", i, got.aliases())
		}
		if got.obj["channel"] != "checkout" {
			t.Fatalf("member %d sees channel %v", i, got.obj["channel"])
		}
	}
}

func TestJoinGivesHintOnceAndRejoinRestores(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	m := w.member()
	first := m.must("join", "checkout", "--as", "form")
	if first.obj["hint"] == nil || first.obj["hint"] == "" {
		t.Fatalf("first join has no hint: %v", first.obj)
	}
	again := m.must("join", "checkout")
	if again.obj["restored"] != true || again.obj["alias"] != "form" {
		t.Fatalf("re-join did not restore: %v", again.obj)
	}
	other := w.member()
	if o := other.run("join", "checkout", "--as", "form"); o.exit != 3 || o.obj["code"] != "alias_taken" {
		t.Fatalf("duplicate alias: exit %d %v", o.exit, o.obj)
	}
	if o := other.run("join", "nowhere"); o.exit != 3 || o.obj["code"] != "not_found" {
		t.Fatalf("unknown channel: exit %d %v", o.exit, o.obj)
	}
	owner.must("owner", "channel", "create", "inventory")
	if o := m.run("join", "inventory"); o.exit != 3 || o.obj["code"] != "one_channel" {
		t.Fatalf("second channel: exit %d %v", o.exit, o.obj)
	}
}

// A retried command returns the original result and changes nothing twice.
func TestRetryReturnsOriginalResult(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	first := owner.must("owner", "channel", "create", "checkout", "--key", "chan-1")
	again := owner.must("owner", "channel", "create", "checkout", "--key", "chan-1")
	if !reflect.DeepEqual(first.obj, again.obj) {
		t.Fatalf("replay differs:\n%v\n%v", first.obj, again.obj)
	}
	if o := owner.run("owner", "channel", "create", "checkout", "--topic", "other", "--key", "chan-1"); o.exit != 3 || o.obj["code"] != "key_conflict" {
		t.Fatalf("changed payload: exit %d %v", o.exit, o.obj)
	}

	m := w.member()
	j1 := m.must("join", "checkout", "--as", "form", "--key", "join-1")
	j2 := m.must("join", "checkout", "--as", "form", "--key", "join-1")
	if !reflect.DeepEqual(j1.obj, j2.obj) {
		t.Fatalf("join replay differs:\n%v\n%v", j1.obj, j2.obj)
	}
	if o := m.run("join", "checkout", "--as", "other", "--key", "join-1"); o.exit != 3 || o.obj["code"] != "key_conflict" {
		t.Fatalf("changed join payload: exit %d %v", o.exit, o.obj)
	}
	if got := m.must("who").aliases(); !reflect.DeepEqual(got, []string{"form"}) {
		t.Fatalf("roster after replays: %v", got)
	}
}

// Killing the dispatcher and starting it again loses no committed row.
func TestKillAndRestartKeepsCommittedRows(t *testing.T) {
	w, owner, members := setup(t)
	w.kill()
	owner.must("owner", "start")
	for i, m := range members {
		if got := sortedAliases(m.must("who")); !reflect.DeepEqual(got, three) {
			t.Fatalf("member %d after restart sees %v", i, got)
		}
	}
}

// A request placed in another member's directory changes nothing. The
// planted request is a focus change that would succeed if it ran as the
// victim, so a resolver that ignored the digest would be caught.
func TestRequestInAnotherMembersLeafChangesNothing(t *testing.T) {
	w, _, members := setup(t)
	victim, intruder := members[1], members[2]
	db := w.db()
	before := snapshot(t, db)

	gen, err := os.ReadFile(filepath.Join(victim.leaf(), "gen"))
	if err != nil {
		t.Fatalf("victim leaf has no gen file: %v", err)
	}
	key := randomHex(16)
	resp, _ := plant(t, victim.leaf(), memberRequest(key, string(gen), intruder.keyDigest(),
		`{"channel":"checkout","focus":"hijacked"}`), false)
	if resp.Status != 4 || resp.Code != "leaf_mismatch" {
		t.Fatalf("misdelivered request answered with status %d code %q", resp.Status, resp.Code)
	}
	if after := snapshot(t, db); after != before {
		t.Fatalf("store changed:\n%+v\n%+v", before, after)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM op WHERE op_key = ?`, key).Scan(&n); err != nil || n != 0 {
		t.Fatalf("planted key recorded %d times (%v)", n, err)
	}
}

// A committed command whose response was lost is retried through the CLI
// with the same key: the original result comes back and nothing is applied
// twice.
func TestLostResponseIsReplayedOnce(t *testing.T) {
	w, owner, members := setup(t)
	db := w.db()

	// Owner: create a channel, drop the response, retry through the CLI.
	key := randomHex(16)
	first, data := plant(t, filepath.Join(w.state, "owner"), ownerRequest(key, `{"name":"lost","topic":"t"}`), true)
	if first.Status != 0 {
		t.Fatalf("planted create failed: %d %s", first.Status, data)
	}
	retry := owner.must("owner", "channel", "create", "lost", "--topic", "t", "--key", key)
	if retry.obj["channel"] != "lost" || retry.obj["topic"] != "t" {
		t.Fatalf("replay %v, original %s", retry.obj, data)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM channel WHERE name = 'lost'`:                                       1,
		`SELECT COUNT(*) FROM event WHERE kind = 'channel_created' AND subject = 'channel:lost'`: 1,
		`SELECT COUNT(*) FROM op WHERE op_key = '` + key + `'`:                                   1,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != want {
			t.Fatalf("%s = %d (%v), want %d", q, n, err, want)
		}
	}

	// Member: a focus change, response dropped, retried through the CLI.
	m := members[0]
	gen, err := os.ReadFile(filepath.Join(m.leaf(), "gen"))
	if err != nil {
		t.Fatal(err)
	}
	mkey := randomHex(16)
	first, data = plant(t, m.leaf(), memberRequest(mkey, string(gen), m.keyDigest(), `{"channel":"checkout","focus":"refunds"}`), true)
	if first.Status != 0 {
		t.Fatalf("planted join failed: %d %s", first.Status, data)
	}
	again := m.must("join", "checkout", "--focus", "refunds", "--key", mkey)
	if again.obj["alias"] != "form" {
		t.Fatalf("replay %v, original %s", again.obj, data)
	}
	var focusChanges int
	if err := db.QueryRow(`SELECT COUNT(*) FROM roster_change WHERE field = 'focus'`).Scan(&focusChanges); err != nil || focusChanges != 1 {
		t.Fatalf("%d focus changes (%v), want 1", focusChanges, err)
	}
}

func TestIdentityRules(t *testing.T) {
	w, _, members := setup(t)
	// Two vendor variables at once are ambiguous.
	twoVendors := &participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=a", "CODEX_THREAD_ID=b"}}
	if o := twoVendors.run("who"); o.exit != 4 || o.obj["code"] != "binding_ambiguous" {
		t.Fatalf("two vendor IDs: exit %d %v", o.exit, o.obj)
	}
	// No identity at all.
	if o := w.owner().run("who"); o.exit != 4 || o.obj["code"] != "not_enrolled" {
		t.Fatalf("no identity: exit %d %v", o.exit, o.obj)
	}
	// A member, or a process with an agent marker, cannot act as the owner.
	marked := &participant{w: w, extra: []string{"CODEX_SESSION_ID=x"}}
	for _, p := range []*participant{members[0], marked} {
		if o := p.run("owner", "channel", "create", "sneaky"); o.exit != 3 || o.obj["code"] != "owner_only" {
			t.Fatalf("owner gate: exit %d %v", o.exit, o.obj)
		}
	}
	if o := members[0].run("join", "sneaky"); o.exit != 3 || o.obj["code"] != "not_found" {
		t.Fatalf("gated create still created the channel: exit %d %v", o.exit, o.obj)
	}
}
