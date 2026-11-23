package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"

	_ "modernc.org/sqlite"
)

type fixture struct {
	t   *testing.T
	db  *sql.DB
	svc *Service
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	migrations, err := filepath.Glob("../store/migrations/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	for _, path := range migrations { // sorted: 001, 002, ...
		schema, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			t.Fatalf("migration %s: %v", path, err)
		}
	}
	f := &fixture{t: t, db: db, now: time.UnixMilli(1_800_000_000_000)}
	f.svc = &Service{Clock: func() time.Time { return f.now }}
	return f
}

// session is a test identity: a vendor key with a leaf, resolved the way the
// dispatcher would before calling the handler.
type session struct {
	vendor, key string
}

func (s session) digest() string {
	sum := sha256.Sum256([]byte(s.vendor + "\x00" + s.key))
	return hex.EncodeToString(sum[:])
}

func (f *fixture) newSession(vendor string) session {
	f.t.Helper()
	s := session{vendor: vendor, key: fmt.Sprintf("key-%d", time.Now().UnixNano())}
	if _, err := f.db.Exec(
		`INSERT INTO leaf (digest, vendor, key, generation, file_identity, created_at_ms) VALUES (?, ?, ?, 'g1', 'fid', ?)`,
		s.digest(), s.vendor, s.key, f.now.UnixMilli()); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// caller resolves a session the way ident does: by its live key.
func (f *fixture) caller(s session) core.Caller {
	var id int64
	err := f.db.QueryRow(`SELECT principal_id FROM principal_key WHERE vendor = ? AND key = ? AND state = 'live'`,
		s.vendor, s.key).Scan(&id)
	if err == sql.ErrNoRows {
		return core.Caller{Role: core.RoleUnenrolled}
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return core.Caller{Role: core.RoleMember, PrincipalID: &id}
}

func enroll(s session) func(context.Context, store.Tx, int64) error {
	return func(ctx context.Context, tx store.Tx, principal int64) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO principal_key (vendor, key, principal_id, created_at_ms) VALUES (?, ?, ?, 0)`,
			s.vendor, s.key, principal); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE leaf SET principal_id = ? WHERE digest = ?`, principal, s.digest())
		return err
	}
}

// exec runs one command in a transaction with the dispatcher's rule: an
// unenrolled caller's refusal commits nothing.
func (f *fixture) exec(caller core.Caller, req Request) core.Result {
	f.t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		f.t.Fatal(err)
	}
	r, err := f.svc.Handle(context.Background(), tx, caller, req)
	if err != nil {
		tx.Rollback()
		f.t.Fatalf("handler error: %v", err)
	}
	if caller.Role == core.RoleUnenrolled && r.Status != core.ExitOK {
		tx.Rollback()
		return r
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// owner runs cmd as the CLI does: it prints the owner line it receives.
func (f *fixture) owner(cmd Command, key string) core.Result {
	return f.exec(core.Caller{Role: core.RoleOwner}, Request{OpKey: key, Command: cmd, OwnerAlerts: true})
}

func (f *fixture) join(s session, cmd Join, key string) core.Result {
	c := f.caller(s)
	req := Request{OpKey: key, Command: cmd, Vendor: s.vendor}
	if c.Role == core.RoleUnenrolled {
		req.Enroll = enroll(s)
	}
	return f.exec(c, req)
}

// joinAck is a join that acknowledges the roster cursor ack.
func (f *fixture) joinAck(s session, cmd Join, ack uint64) core.Result {
	return f.exec(f.caller(s), Request{Command: cmd, Vendor: s.vendor, Ack: ack})
}

func (f *fixture) who(s session) core.Result {
	return f.exec(f.caller(s), Request{Command: Who{}})
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func wantOK(t *testing.T, r core.Result) {
	t.Helper()
	if r.Status != core.ExitOK {
		t.Fatalf("status %d code %s data %s", r.Status, r.Code, r.Data)
	}
}

func wantCode(t *testing.T, r core.Result, status core.ExitCode, code core.Code) {
	t.Helper()
	if r.Status != status || r.Code != code {
		t.Fatalf("got status %d code %q (%s), want %d %q", r.Status, r.Code, r.Data, status, code)
	}
}

func decode[T any](t *testing.T, r core.Result) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func aliases(ms []Member) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Alias)
	}
	return out
}

func TestCreateChannel(t *testing.T) {
	f := newFixture(t)
	r := f.owner(CreateChannel{Name: "checkout", Topic: "payment screen"}, "k1")
	wantOK(t, r)
	if got := decode[ChannelResult](t, r); got != (ChannelResult{Channel: "checkout", Topic: "payment screen"}) {
		t.Fatalf("result %+v", got)
	}
	wantCode(t, f.owner(CreateChannel{Name: "checkout"}, "k2"), core.ExitRefused, core.CodeChannelExists)

	again := f.owner(CreateChannel{Name: "checkout", Topic: "payment screen"}, "k1")
	if !reflect.DeepEqual(again, r) {
		t.Fatalf("replay differs: %+v vs %+v", again, r)
	}
	wantCode(t, f.owner(CreateChannel{Name: "checkout", Topic: "other"}, "k1"), core.ExitRefused, core.CodeKeyConflict)
	if n := f.count(`SELECT COUNT(*) FROM channel`); n != 1 {
		t.Fatalf("%d channels", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM principal WHERE kind = 'owner'`); n != 1 {
		t.Fatalf("%d owner principals", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'channel_created'`); n != 1 {
		t.Fatalf("%d audit rows", n)
	}
}

func TestOnlyTheOwnerCreatesChannels(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	s := f.newSession("claude")
	wantOK(t, f.join(s, Join{Channel: "checkout"}, "j"))
	r := f.exec(f.caller(s), Request{OpKey: "c", Command: CreateChannel{Name: "sneaky"}})
	wantCode(t, r, core.ExitRefused, core.CodeOwnerOnly)
	wantCode(t, f.exec(core.Caller{Role: core.RoleOwner}, Request{Command: Join{Channel: "checkout"}}),
		core.ExitRefused, core.CodeOwnerOnly)
}

func TestFirstJoinCreatesPrincipalAndLinksKey(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout", Topic: "payment screen"}, "")
	s := f.newSession("codex")
	r := f.join(s, Join{Channel: "checkout", Alias: "tax", Focus: "rounding"}, "j1")
	wantOK(t, r)
	got := decode[JoinResult](t, r)
	if got.Alias != "tax" || got.Restored || got.Hint == "" || got.Topic != "payment screen" {
		t.Fatalf("result %+v", got)
	}
	if len(got.Members) != 1 || !got.Members[0].Self || got.Members[0].Focus != "rounding" || got.Members[0].Vendor != "codex" {
		t.Fatalf("members %+v", got.Members)
	}
	c := f.caller(s)
	if c.Role != core.RoleMember {
		t.Fatal("key not linked")
	}
	if n := f.count(`SELECT COUNT(*) FROM leaf WHERE principal_id = ?`, *c.PrincipalID); n != 1 {
		t.Fatal("leaf not linked")
	}
	if n := f.count(`SELECT round_limit FROM principal WHERE id = ?`, *c.PrincipalID); n != guardDefaultRounds {
		t.Fatalf("new principal round limit = %d", n)
	}

}

func TestFailedFirstJoinLeavesNothing(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	first := f.newSession("claude")
	wantOK(t, f.join(first, Join{Channel: "checkout", Alias: "form"}, ""))
	before := f.count(`SELECT COUNT(*) FROM principal`)

	s := f.newSession("claude")
	r := f.join(s, Join{Channel: "chekout"}, "x")
	wantCode(t, r, core.ExitRefused, core.CodeNotFound)
	if hint := decode[Refusal](t, r).Hint; hint != "crosshall join checkout" {
		t.Fatalf("closest-match hint %q", hint)
	}
	wantCode(t, f.join(s, Join{Channel: "checkout", Alias: "form"}, "y"), core.ExitRefused, core.CodeAliasTaken)
	wantCode(t, f.join(s, Join{Channel: "checkout", Alias: "owner"}, "z"), core.ExitRefused, core.CodeAliasTaken)
	if after := f.count(`SELECT COUNT(*) FROM principal`); after != before {
		t.Fatalf("principals %d -> %d", before, after)
	}
	if n := f.count(`SELECT COUNT(*) FROM principal_key WHERE key = ?`, s.key); n != 0 {
		t.Fatal("refused join linked its key")
	}
}

func TestHandlerRefusesBeforeInserting(t *testing.T) {
	// Even if a dispatcher committed a refused first join, the handler must
	// not have created anything before refusing.
	f := newFixture(t)
	s := f.newSession("claude")
	tx, _ := f.db.Begin()
	r, err := f.svc.Handle(context.Background(), tx, core.Caller{Role: core.RoleUnenrolled},
		Request{Command: Join{Channel: "nowhere"}, Vendor: "claude", Enroll: enroll(s)})
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, r, core.ExitRefused, core.CodeNotFound)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT COUNT(*) FROM principal`); n != 0 {
		t.Fatalf("%d principals after a refused join", n)
	}
}

func TestRejoinAndOneChannel(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	f.owner(CreateChannel{Name: "inventory"}, "")
	s := f.newSession("claude")
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: "form"}, ""))

	again := decode[JoinResult](t, f.join(s, Join{Channel: "checkout"}, ""))
	if !again.Restored || again.Alias != "form" || again.Hint == "" {
		t.Fatalf("unacknowledged re-join must repeat the hint: %+v", again)
	}
	wantCode(t, f.join(s, Join{Channel: "checkout", Alias: "other"}, ""), core.ExitRefused, core.CodeAliasTaken)
	wantCode(t, f.join(s, Join{Channel: "inventory"}, ""), core.ExitRefused, core.CodeOneChannel)

	// Departure permits a new channel. Returning to the original channel
	// keeps its binding and alias.
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed', departed_at_ms = 1`); err != nil {
		t.Fatal(err)
	}
	wantOK(t, f.join(s, Join{Channel: "inventory"}, ""))
	back := decode[JoinResult](t, f.join(s, Join{Channel: "checkout", Move: true}, ""))
	if back.Restored || back.Alias != "form" || back.Hint == "" {
		t.Fatalf("return after departure %+v", back)
	}
	if n := f.count(`SELECT COUNT(*) FROM binding`); n != 2 {
		t.Fatalf("%d bindings", n)
	}
}

func TestDepartedAliasCanBeReused(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	a, b := f.newSession("claude"), f.newSession("claude")
	wantOK(t, f.join(a, Join{Channel: "checkout", Alias: "form"}, ""))
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed'`); err != nil {
		t.Fatal(err)
	}
	wantOK(t, f.join(b, Join{Channel: "checkout", Alias: "form"}, ""))
	// The first session cannot come back under the reused alias.
	wantCode(t, f.join(a, Join{Channel: "checkout"}, ""), core.ExitRefused, core.CodeAliasTaken)
}

func TestGeneratedAliases(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	var got []string
	for _, v := range []string{"claude", "claude", "route", "Weird Vendor"} {
		got = append(got, decode[JoinResult](t, f.join(f.newSession(v), Join{Channel: "checkout"}, "")).Alias)
	}
	if want := []string{"claude1", "claude2", "route1", "member1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases %v, want %v", got, want)
	}
}

func TestWho(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout", Topic: "payment screen"}, "")
	f.owner(CreateChannel{Name: "inventory"}, "")
	a, b, c := f.newSession("claude"), f.newSession("codex"), f.newSession("agy")
	wantOK(t, f.join(a, Join{Channel: "checkout", Alias: "form"}, ""))
	f.now = f.now.Add(time.Minute)
	wantOK(t, f.join(b, Join{Channel: "checkout", Alias: "tax"}, ""))
	wantOK(t, f.join(c, Join{Channel: "inventory", Alias: "stock"}, ""))

	f.now = f.now.Add(9 * time.Minute)
	got := decode[WhoResult](t, f.who(b))
	if got.Channel != "checkout" || got.Topic != "payment screen" {
		t.Fatalf("who %+v", got)
	}
	if want := []string{"tax", "form"}; !reflect.DeepEqual(aliases(got.Members), want) {
		t.Fatalf("members %v, want %v (self first, then join order)", aliases(got.Members), want)
	}
	if m := got.Members[1]; m.LastCommandAgoS != 600 || m.State != "unknown" || m.Route != "pull" || m.Self {
		t.Fatalf("row %+v", m)
	}
	// The who itself is an observed command of the caller.
	if got.Members[0].LastCommandAgoS != 0 {
		t.Fatalf("caller age %d", got.Members[0].LastCommandAgoS)
	}

	wantCode(t, f.who(f.newSession("claude")), core.ExitNotEnrolled, core.CodeNotEnrolled)
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE alias = 'form'`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.who(a), core.ExitNotEnrolled, core.CodeNotEnrolled)
	if names := aliases(decode[WhoResult](t, f.who(b)).Members); !reflect.DeepEqual(names, []string{"tax"}) {
		t.Fatalf("departed member still listed: %v", names)
	}
}

func TestJoinRosterIsCapped(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "big"}, "")
	var last JoinResult
	for i := 0; i < 15; i++ {
		f.now = f.now.Add(time.Second)
		last = decode[JoinResult](t, f.join(f.newSession("route"), Join{Channel: "big"}, ""))
	}
	if len(last.Members) != RosterCap || last.Omitted != 3 || !last.Members[0].Self {
		t.Fatalf("%d rows, %d omitted, first %+v", len(last.Members), last.Omitted, last.Members[0])
	}
}

func TestJoinReplayReturnsTheOriginalResult(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	s := f.newSession("claude")
	first := f.join(s, Join{Channel: "checkout", Alias: "form"}, "j1")
	wantOK(t, first)
	wantOK(t, f.join(f.newSession("codex"), Join{Channel: "checkout"}, ""))
	f.now = f.now.Add(time.Hour)

	again := f.join(s, Join{Channel: "checkout", Alias: "form"}, "j1")
	if !reflect.DeepEqual(again, first) {
		t.Fatalf("replay differs:\n%s\n%s", again.Data, first.Data)
	}
	wantCode(t, f.join(s, Join{Channel: "checkout", Alias: "form", Focus: "x"}, "j1"), core.ExitRefused, core.CodeKeyConflict)
	if n := f.count(`SELECT COUNT(*) FROM binding`); n != 2 {
		t.Fatalf("%d bindings", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'joined'`); n != 2 {
		t.Fatalf("%d join audit rows", n)
	}
}

func TestArchivedChannelRefusesJoin(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "old"}, "")
	if _, err := f.db.Exec(`UPDATE channel SET state = 'archived'`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.join(f.newSession("claude"), Join{Channel: "old"}, ""), core.ExitRefused, core.CodeArchived)
}

func TestJoinOutputRepeatsUntilAcknowledged(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	a, b, c := f.newSession("claude"), f.newSession("codex"), f.newSession("agy")
	first := decode[JoinResult](t, f.join(a, Join{Channel: "checkout", Alias: "form"}, ""))
	if first.RosterSeq == 0 || !strings.Contains(first.Hint, "Act on outstanding work before rearming:") {
		t.Fatalf("first join %+v", first)
	}
	// The first response was lost: a re-join with a fresh key and no ack
	// gives the hint and the full roster again.
	lost := decode[JoinResult](t, f.join(a, Join{Channel: "checkout"}, ""))
	if lost.Hint == "" || len(lost.Members) != 1 {
		t.Fatalf("re-join after a lost response %+v", lost)
	}
	// Once acknowledged, a re-join says "Already in" and names the command
	// that prints the rules instead of repeating them, and of the roster
	// shows only the changes after the cursor.
	acked := decode[JoinResult](t, f.joinAck(a, Join{Channel: "checkout"}, lost.RosterSeq))
	if !strings.HasPrefix(acked.Hint, "[crosshall] Already in #checkout as @form.\n") ||
		!strings.Contains(acked.Hint, "`crosshall join checkout --rules`") || strings.Contains(acked.Hint, "Later arrivals belong to the next turn.") ||
		len(acked.Hint) > 160 || len(acked.Members) != 0 || len(acked.Changes) != 0 {
		t.Fatalf("acknowledged re-join %+v", acked)
	}
	// A member that lost its context asks for the rules.
	asked := decode[JoinResult](t, f.joinAck(a, Join{Channel: "checkout", Rules: true}, lost.RosterSeq))
	if !strings.HasPrefix(asked.Hint, "[crosshall] Already in #checkout as @form.\n") ||
		!strings.Contains(asked.Hint, "Later arrivals belong to the next turn.") || !strings.Contains(asked.Hint, "Act on outstanding work before rearming:") {
		t.Fatalf("re-join asking for the rules %+v", asked)
	}
	// The dispatcher records the ack after the handler; simulate that.
	if _, err := f.db.Exec(`UPDATE binding SET roster_ack_seq = ?, hint_acked = 1 WHERE alias = 'form'`, acked.RosterSeq); err != nil {
		t.Fatal(err)
	}
	wantOK(t, f.join(b, Join{Channel: "checkout", Alias: "tax"}, ""))
	wantOK(t, f.join(c, Join{Channel: "checkout", Alias: "a11y", Focus: "contrast"}, ""))
	next := decode[JoinResult](t, f.join(a, Join{Channel: "checkout"}, ""))
	if !strings.HasPrefix(next.Hint, "[crosshall] Already in #checkout as @form.") || len(next.Changes) != 2 || next.Changes[0].Alias != "tax" ||
		next.Changes[1].Alias != "a11y" || next.Changes[1].Change != "joined" || next.Changes[1].Focus != "contrast" {
		t.Fatalf("changes after peers joined %+v", next.Changes)
	}
	// An ack carried by this request already counts: nothing new is shown.
	again := decode[JoinResult](t, f.joinAck(a, Join{Channel: "checkout"}, next.RosterSeq))
	if len(again.Changes) != 0 {
		t.Fatalf("acknowledged changes repeated %+v", again.Changes)
	}
	// An ack beyond the current sequence is bounded and shows nothing extra.
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE alias = 'tax'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO roster_change (channel_id, seq, principal_id, field)
		SELECT channel_id, (SELECT MAX(seq) + 1 FROM roster_change), principal_id, 'departed' FROM binding WHERE alias = 'tax'`); err != nil {
		t.Fatal(err)
	}
	gone := decode[JoinResult](t, f.joinAck(a, Join{Channel: "checkout"}, next.RosterSeq))
	if len(gone.Changes) != 1 || gone.Changes[0].Alias != "tax" || gone.Changes[0].Change != "departed" {
		t.Fatalf("departure not shown %+v", gone.Changes)
	}
	// A second session that took the freed alias and left as well reads as
	// the same line, so it is shown once.
	d := f.newSession("codex")
	wantOK(t, f.join(d, Join{Channel: "checkout", Alias: "tax"}, ""))
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE alias = 'tax'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO roster_change (channel_id, seq, principal_id, field)
		SELECT channel_id, (SELECT MAX(seq) + 1 FROM roster_change), principal_id, 'departed' FROM binding
		WHERE alias = 'tax' ORDER BY id DESC LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	twice := decode[JoinResult](t, f.joinAck(a, Join{Channel: "checkout"}, next.RosterSeq))
	if len(twice.Changes) != 1 || twice.Changes[0].Alias != "tax" || twice.Changes[0].Change != "departed" {
		t.Fatalf("a reused alias departed more than once for the reader: %+v", twice.Changes)
	}
}

func TestWhoCarriesRosterCursor(t *testing.T) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	s := f.newSession("claude")
	joined := decode[JoinResult](t, f.join(s, Join{Channel: "checkout"}, ""))
	// Before the join output is acknowledged, who must not acknowledge it.
	if got := decode[WhoResult](t, f.who(s)); got.RosterSeq != 0 {
		t.Fatalf("who advanced the cursor before the hint was acknowledged: %d", got.RosterSeq)
	}
	// A who that carries the ack may return the cursor.
	got := decode[WhoResult](t, f.exec(f.caller(s), Request{Command: Who{}, Ack: joined.RosterSeq}))
	if got.RosterSeq != 1 {
		t.Fatalf("roster_seq %d", got.RosterSeq)
	}
}

func TestLineBreaksAreRejected(t *testing.T) {
	f := newFixture(t)
	wantCode(t, f.owner(CreateChannel{Name: "checkout", Topic: "a\nb"}, ""), core.ExitUsage, "")
	f.owner(CreateChannel{Name: "checkout"}, "")
	s := f.newSession("claude")
	wantCode(t, f.join(s, Join{Channel: "checkout", Focus: "x\r\ny"}, ""), core.ExitUsage, "")
	wantOK(t, f.join(s, Join{Channel: "checkout", Focus: "one line"}, ""))
	wantCode(t, f.join(s, Join{Channel: "checkout", Focus: "tab\there"}, ""), core.ExitUsage, "")
	if n := f.count(`SELECT COUNT(*) FROM channel`); n != 1 {
		t.Fatalf("%d channels", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE focus = 'one line'`); n != 1 {
		t.Fatal("focus overwritten by a refused join")
	}
}
