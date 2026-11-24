package domain

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

const window = store.RetentionWindow

func (f *fixture) prune() {
	f.t.Helper()
	f.inTx(func(tx store.Tx) error {
		_, err := store.PruneRetentionTx(context.Background(), tx, f.now)
		return err
	})
}

func (f *fixture) departToken(binding, principal int64) string {
	f.t.Helper()
	var token string
	f.inTx(func(tx store.Tx) (err error) {
		token, err = departureToken(context.Background(), tx, binding, principal)
		return err
	})
	return token
}

type turnRow struct {
	ref       string
	opened    int64
	finishing int64
}

func (f *fixture) turns(principal int64, launch string) []turnRow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT turn_ref, opened_hook_at_ms, COALESCE(finishing_hook_at_ms, 0) FROM turn
		WHERE principal_id = ? AND launch_ref = ? ORDER BY opened_hook_at_ms, turn_ref`, principal, launch)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []turnRow
	for rows.Next() {
		var r turnRow
		if err := rows.Scan(&r.ref, &r.opened, &r.finishing); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// The cut is the start of the pass's UTC day less the window: an op at the
// cut is kept and replays, one a millisecond older is deleted, and a second
// pass the same day deletes nothing more. Past the cut, the same key runs
// as a new command (#256).
func TestRetentionDeletesOpsOlderThanTheWindowAndTheirKeysRunAnew(t *testing.T) {
	f, writer := adapterChannel(t)
	reader := f.newSession("claude")
	wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, ""))
	y, m, d := f.now.UTC().Date()
	cut := time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
	f.now = cut.Add(-time.Millisecond)
	wantOK(t, f.run(writer, Note{To: []string{"reader"}, Body: "older"}, "older-key"))
	f.now = cut
	note := Note{To: []string{"reader"}, Body: "at the cut"}
	first := decode[NoteResult](t, f.run(writer, note, "note-key")).ID

	for _, now := range []time.Time{cut.Add(window), cut.Add(window + 24*time.Hour - time.Millisecond)} {
		f.now = now
		f.prune()
		if n := f.count(`SELECT COUNT(*) FROM op WHERE op_key = 'older-key'`); n != 0 {
			t.Fatalf("an op older than the cut was kept (%d)", n)
		}
		if n := f.count(`SELECT COUNT(*) FROM op WHERE op_key = 'note-key'`); n != 1 {
			t.Fatalf("an op at the cut was deleted on the pass at %v", now)
		}
	}
	if again := decode[NoteResult](t, f.run(writer, note, "note-key")).ID; again != first {
		t.Fatalf("a key inside the window ran again: %d, want the replay of %d", again, first)
	}

	f.now = cut.Add(window + 24*time.Hour)
	f.prune()
	if n := f.count(`SELECT COUNT(*) FROM op WHERE op_key = 'note-key'`); n != 0 {
		t.Fatalf("an op older than the next day's cut was kept (%d)", n)
	}
	if anew := decode[NoteResult](t, f.run(writer, note, "note-key")).ID; anew == first {
		t.Fatal("a key past the window still replayed")
	}
}

// A principal's latest hook op stays at any age, so the departure token
// listed before a pass still departs after it, and the next hook op takes
// a larger rowid, which changes the token.
func TestRetentionKeepsTheLatestHookOpAndTheDepartureToken(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(member, start("pid-1", "startup", t0))
	f.hook(member, prompt("pid-1", "turn-1", t0+1))
	f.hook(member, stop("pid-1", "turn-1", t0+2))
	principal := *f.caller(member).PrincipalID
	binding := bindingIDOf(t, f, principal)
	latest := f.count(`SELECT MAX(rowid) FROM op WHERE principal_id = ? AND scope = 'hook'`, principal)
	token := f.departToken(binding, principal)

	f.now = f.now.Add(window + 24*time.Hour)
	f.prune()
	if n := f.count(`SELECT COUNT(*) FROM op WHERE principal_id = ? AND scope = 'hook'`, principal); n != 1 {
		t.Fatalf("%d hook ops left, want only the latest", n)
	}
	if got := f.count(`SELECT MAX(rowid) FROM op WHERE principal_id = ? AND scope = 'hook'`, principal); got != latest {
		t.Fatalf("latest hook op %d, want %d", got, latest)
	}
	if got := f.departToken(binding, principal); got != token {
		t.Fatal("a pass changed the departure token")
	}

	f.hook(member, prompt("pid-1", "turn-2", f.now.UnixMilli()))
	if got := f.count(`SELECT MAX(rowid) FROM op WHERE principal_id = ? AND scope = 'hook'`, principal); got <= latest {
		t.Fatalf("a new hook op took rowid %d, not above the kept %d", got, latest)
	}
	if f.departToken(binding, principal) == token {
		t.Fatal("new activity after a pass did not change the departure token")
	}
}

// Old commands go except each principal's newest, which the views read,
// and the principal's newest row, which the departure token reads.
func TestRetentionKeepsTheNewestCommandAndTheNewestRow(t *testing.T) {
	f, writer := adapterChannel(t)
	reader := f.newSession("claude")
	wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, ""))
	principal := *f.caller(writer).PrincipalID
	binding := bindingIDOf(t, f, principal)
	f.noteTo(writer, "reader", 3)
	f.now = f.now.Add(time.Hour)
	f.noteTo(writer, "reader", 1)
	newest := f.count(`SELECT seq FROM observation WHERE principal_id = ? AND kind = 'command'
		ORDER BY observed_at_ms DESC, seq DESC LIMIT 1`, principal)
	if n := f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ? AND kind = 'command'`, principal); n < 2 {
		t.Fatalf("fixture recorded %d commands", n)
	}
	f.now = f.now.Add(window + 24*time.Hour)
	before := f.who(reader).Data
	token := f.departToken(binding, principal)
	f.prune()
	if n := f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ?`, principal); n != 1 {
		t.Fatalf("%d observations left, want the newest command only", n)
	}
	if got := f.count(`SELECT seq FROM observation WHERE principal_id = ?`, principal); got != newest {
		t.Fatalf("kept observation %d, want the newest command %d", got, newest)
	}
	if after := f.who(reader).Data; string(after) != string(before) {
		t.Fatalf("who changed across the pass:\n%s\n%s", before, after)
	}
	if f.departToken(binding, principal) != token {
		t.Fatal("a pass changed the departure token")
	}
}

// Every fact of a launch still running for a live member stays at any age:
// the ledger rebuilds the same turns from them, open placeholder included.
func TestRetentionKeepsEveryFactOfARunningLaunch(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(member, start("pid-1", "startup", t0))
	f.hook(member, prompt("pid-1", "user", t0+1))
	f.hook(member, event("pid-1", FactPromptSubmitted, "user:repeated", "user", t0+2, true))
	f.hook(member, stop("pid-1", "user", t0+3))
	f.hook(member, prompt("pid-1", "long", t0+4)) // runs on
	principal := *f.caller(member).PrincipalID
	facts := f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ? AND launch_ref = 'pid-1'`, principal)
	before := f.turns(principal, "pid-1")
	if !slices.ContainsFunc(before, func(r turnRow) bool {
		return r.finishing == 0 && len(r.ref) > len(typedAheadPrefix) &&
			r.ref[:len(typedAheadPrefix)] == typedAheadPrefix
	}) {
		t.Fatalf("fixture has no open placeholder: %+v", before)
	}

	f.now = f.now.Add(window + 24*time.Hour)
	f.prune()
	if n := f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ? AND launch_ref = 'pid-1'`, principal); n != facts {
		t.Fatalf("%d of %d facts of a running launch left", n, facts)
	}
	f.hook(member, stop("pid-1", "long", f.now.UnixMilli()))
	after := f.turns(principal, "pid-1")
	for _, r := range before {
		if r.ref == "long" {
			continue
		}
		if !slices.Contains(after, r) {
			t.Fatalf("turn %+v changed after the pass: %+v", r, after)
		}
	}
}

// An ended launch loses its facts and its unclaimed turns; a turn that a
// turn-end notice references stays. A resume of the same launch builds its
// turns from new facts.
func TestRetentionClearsAnEndedLaunchAndItsResumeStartsOver(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(member, start(SessionLaunch, "startup", t0))
	f.hook(member, prompt(SessionLaunch, "claimed", t0+1))
	f.hook(member, stop(SessionLaunch, "claimed", t0+2))
	f.hook(member, prompt(SessionLaunch, "plain", t0+3))
	f.hook(member, stop(SessionLaunch, "plain", t0+4))
	principal := *f.caller(member).PrincipalID
	if _, err := f.db.Exec(`INSERT INTO turn_end_notice (principal_id, adapter, launch_ref, turn_ref, at_ms)
		VALUES (?, 'claude', ?, 'claimed', ?)`, principal, SessionLaunch, t0+2); err != nil {
		t.Fatal(err)
	}
	// A session-scoped launch records no end; its session_ended fact since
	// its start ends it for retention.
	f.hook(member, ended(SessionLaunch, t0+5))
	if f.count(`SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ? AND ended_hook_at_ms IS NULL`, principal) != 1 {
		t.Fatal("fixture: the session-scoped launch recorded an end")
	}

	f.now = f.now.Add(window + 24*time.Hour)
	f.prune()
	if n := f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ? AND kind IN ('prompt_submitted', 'turn_finishing')`,
		principal); n != 0 {
		t.Fatalf("%d prompt and Stop facts of an ended launch left", n)
	}
	if got := f.turns(principal, SessionLaunch); len(got) != 1 || got[0].ref != "claimed" {
		t.Fatalf("turns after the pass %+v, want only the claimed one", got)
	}

	at := f.now.UnixMilli()
	f.hook(member, start(SessionLaunch, "resume", at))
	f.hook(member, prompt(SessionLaunch, "fresh", at+1))
	f.hook(member, stop(SessionLaunch, "fresh", at+2))
	var refs []string
	for _, r := range f.turns(principal, SessionLaunch) {
		refs = append(refs, r.ref)
	}
	if !slices.Equal(refs, []string{"claimed", "fresh"}) {
		t.Fatalf("turns after the resume %v, want the claimed one and the fresh one", refs)
	}
}

// A principal with no live binding keeps only its newest row and newest
// command, also for a launch that never recorded an end; it rebuilds its
// ledger from new facts when it returns.
func TestRetentionClearsADepartedPrincipal(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(member, start("pid-1", "startup", t0))
	f.hook(member, prompt("pid-1", "old", t0+1))
	f.hook(member, stop("pid-1", "old", t0+2))
	wantOK(t, f.run(member, Leave{}, ""))
	principal := *f.caller(member).PrincipalID
	if f.count(`SELECT COUNT(*) FROM binding WHERE principal_id = ? AND state = 'live'`, principal) != 0 {
		t.Fatal("the member did not leave")
	}

	keep := f.count(`SELECT COUNT(DISTINCT seq) FROM observation WHERE seq IN (
		(SELECT MAX(seq) FROM observation WHERE principal_id = ?1),
		(SELECT seq FROM observation WHERE principal_id = ?1 AND kind = 'command' ORDER BY observed_at_ms DESC, seq DESC LIMIT 1))`,
		principal)
	if f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ? AND kind <> 'command'`, principal) == 0 {
		t.Fatal("fixture has no hook facts")
	}

	f.now = f.now.Add(window + 24*time.Hour)
	f.prune()
	if kept := f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ?`, principal); kept != keep {
		t.Fatalf("%d observations left, want the newest row and the newest command (%d)", kept, keep)
	}
	if got := f.turns(principal, "pid-1"); len(got) != 0 {
		t.Fatalf("turns of a departed principal left: %+v", got)
	}

	wantOK(t, f.join(member, Join{Channel: "checkout"}, ""))
	at := f.now.UnixMilli()
	f.hook(member, prompt("pid-1", "new", at))
	f.hook(member, stop("pid-1", "new", at+1))
	if got := f.turns(principal, "pid-1"); len(got) != 1 || got[0].ref != "new" {
		t.Fatalf("turns after the return %+v, want only the new one", got)
	}
}

// A live member idle past the window still has its last fact.
func TestRetentionKeepsTheLastFactOfAnIdleMember(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(member, start(SessionLaunch, "startup", t0))
	f.hook(member, prompt(SessionLaunch, "turn", t0+1))
	f.hook(member, stop(SessionLaunch, "turn", t0+2))
	f.hook(member, ended(SessionLaunch, t0+3))
	principal := *f.caller(member).PrincipalID
	latest := func() *ObservedFact {
		var o *ObservedFact
		f.inTx(func(tx store.Tx) (err error) {
			o, err = latestFact(context.Background(), tx, principal)
			return err
		})
		return o
	}
	before := latest()
	if before == nil {
		t.Fatal("fixture has no last fact")
	}

	f.now = f.now.Add(window + 24*time.Hour)
	f.prune()
	if after := latest(); after == nil || after.Fact != before.Fact || after.AtMS != before.AtMS || after.Source != before.Source {
		t.Fatalf("last fact %+v after the pass, want %+v", after, before)
	}
}
