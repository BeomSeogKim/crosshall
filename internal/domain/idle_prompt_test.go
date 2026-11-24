package domain

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func idlePrompt(launch string, at int64) HookEvent {
	return event(launch, FactIdlePrompt, fmt.Sprintf("idle:%d", at), "", at, false)
}

// absorbTypedPrompt is Claude Code 2.1.330's shape for a prompt typed while
// a tool runs: the prompt reports the running turn's ID and rides with the
// tool result, so the turn's one Stop is all that follows (#236 PR 6).
func (f *fixture) absorbTypedPrompt(s session, launch, turn string, at int64) {
	f.t.Helper()
	f.hook(s, prompt(launch, turn, at))
	f.hook(s, event(launch, FactPromptSubmitted, turn+":typed", turn, at+1, true))
	f.hook(s, stop(launch, turn, at+2))
}

func (f *fixture) openPlaceholders(principal int64, launch string) int {
	f.t.Helper()
	return f.count(`SELECT COUNT(*) FROM turn WHERE principal_id = ? AND launch_ref = ?
		AND turn_ref LIKE 'typed-ahead:%' AND finishing_hook_at_ms IS NULL`, principal, launch)
}

// A notice to a session that absorbed a typed prompt is unknown until the
// session reports it waits for input. The idle fact closes the placeholder
// and lifts the suspension, so the next notice is the session's own turn.
func TestIdlePromptRepairsTypedAheadAndUnknownSuspension(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	started := p.now.UnixMilli()
	principal := p.principalOf(p.reader)
	p.absorbTypedPrompt(p.reader, publishTestLaunch, "tool", started+1)
	if n := p.openPlaceholders(principal, publishTestLaunch); n != 1 {
		t.Fatalf("fixture: %d open placeholders after the absorbed prompt", n)
	}

	p.turnEndAsk(p.writer, "reader", "before-idle")
	attempt := p.reserve()
	if attempt == 0 {
		t.Fatal("the first notice was not reserved")
	}
	ids := p.begin(attempt)
	p.resolve(attempt, PublishSubmitted)
	p.hook(p.reader, p.promptReceipt("first-notice", publishTestLaunch, started+4, ids))
	p.observation(attempt, "unknown")
	p.unknownCause(attempt, "open_placeholder")
	p.hook(p.reader, stop(publishTestLaunch, "first-notice", started+5))
	p.requirePublish("before idle", "suspended", "unknown")

	p.now = p.now.Add(time.Minute)
	at := p.now.UnixMilli()
	if out := p.hook(p.reader, idlePrompt(publishTestLaunch, at)); !quiet(out) {
		t.Fatalf("idle callback produced output: %+v", out)
	}
	p.hook(p.reader, idlePrompt(publishTestLaunch, at))
	if n := p.count(`SELECT COUNT(*) FROM observation WHERE kind = 'idle_prompt'`); n != 1 {
		t.Fatalf("idle observation count %d, want 1", n)
	}
	if n := p.openPlaceholders(principal, publishTestLaunch); n != 0 {
		t.Fatalf("%d placeholders remained open at idle", n)
	}
	p.observation(attempt, "unknown")

	p.turnEndAsk(p.writer, "reader", "after-idle")
	p.requirePublish("after idle", "available", "")
	next := p.reserve()
	if next == 0 {
		t.Fatal("the second notice was not reserved")
	}
	ids = p.begin(next)
	p.hook(p.reader, p.promptReceipt("second-notice", publishTestLaunch, at+2, ids))
	p.observation(next, "own_turn")
}

// The idle fact ends the placeholder it finds open; a later prompt with a
// new ID opens a turn of its own instead of taking the placeholder's
// place, and no placeholder opens for it.
func TestIdlePromptEndsThePlaceholderSoTheNextTurnIsItsOwn(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(member, start("pid-1", "startup", t0))
	principal := *f.caller(member).PrincipalID
	f.absorbTypedPrompt(member, "pid-1", "tool", t0+1)
	f.now = time.UnixMilli(t0 + 60_000)
	f.hook(member, idlePrompt("pid-1", t0+60_000))
	f.hook(member, prompt("pid-1", "next", t0+70_000))
	f.hook(member, stop("pid-1", "next", t0+71_000))

	if n := f.count(`SELECT COUNT(*) FROM turn WHERE principal_id = ? AND launch_ref = 'pid-1'
		AND finishing_hook_at_ms IS NULL`, principal); n != 0 {
		t.Fatalf("%d turns still open after the idle fact and the next Stop", n)
	}
	rows, err := f.db.Query(`SELECT turn_ref FROM turn WHERE principal_id = ? AND launch_ref = 'pid-1'
		ORDER BY opened_hook_at_ms, turn_ref`, principal)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	if len(refs) != 3 || refs[0] != "tool" || refs[2] != "next" || !slices.ContainsFunc(refs[1:2], func(ref string) bool {
		return len(ref) > len(typedAheadPrefix) && ref[:len(typedAheadPrefix)] == typedAheadPrefix
	}) {
		t.Fatalf("turns %v, want the tool turn, its closed placeholder and the next turn", refs)
	}
}

// Only an unknown suspension committed before the idle fact ends at it: a
// later one, and an absorbed or not-submitted one, keep their terms.
func TestIdlePromptLiftsOnlyAnUnknownSuspensionOlderThanIt(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	t0 := p.now.UnixMilli()
	until := t0 + store.PublishSuspensionDuration.Milliseconds()
	for _, s := range []struct {
		reason string
		at     int64
		until  any
	}{{"unknown", t0, until}, {"unknown", t0 + 20_000, until}, {"absorbed", t0, until}, {"not_submitted", t0, until},
		{"unknown", t0 + 1, nil}} {
		p.mutate(`INSERT INTO publish_suspension (binding_id, launch_ref, reason, notice, at_ms, until_ms)
			VALUES (?, ?, ?, 0, ?, ?)`, p.binding.ID, publishTestLaunch, s.reason, s.at, s.until)
	}
	p.now = p.now.Add(30 * time.Second)
	p.hook(p.reader, idlePrompt(publishTestLaunch, t0+10_000))
	rows, err := p.db.Query(`SELECT reason, at_ms, until_ms FROM publish_suspension ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var reason string
		var at int64
		var end sql.NullInt64
		if err := rows.Scan(&reason, &at, &end); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s+%d:%t", reason, at-t0, end.Int64 == p.now.UnixMilli()))
	}
	want := []string{"unknown+0:true", "unknown+20000:false", "absorbed+0:false", "not_submitted+0:false", "unknown+1:true"}
	if !slices.Equal(got, want) {
		t.Fatalf("suspensions after the idle fact %v, want %v (reason+at:lifted)", got, want)
	}
	var events []string
	erows, err := p.db.Query(`SELECT subject, json_extract(detail, '$.suspension'), json_extract(detail, '$.lasting'),
		json_extract(detail, '$.by') FROM event WHERE kind = 'publish_suspension_lifted' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer erows.Close()
	for erows.Next() {
		var subject, by string
		var suspension, lasting int64
		if err := erows.Scan(&subject, &suspension, &lasting, &by); err != nil {
			t.Fatal(err)
		}
		events = append(events, fmt.Sprintf("%s %d %d %s", subject, suspension, lasting, by))
	}
	binding := fmt.Sprintf("binding:%d", p.binding.ID)
	if want := []string{binding + " 1 0 idle_prompt", binding + " 5 1 idle_prompt"}; !slices.Equal(events, want) {
		t.Fatalf("lift events %v, want %v", events, want)
	}
}

// An idle fact counts only for the member's current bound launch: one from
// a session-scoped launch, or from a launch a newer one replaced, is not
// stored and closes nothing.
func TestIdlePromptOfAnotherLaunchChangesNothing(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(member, start("pid-1", "startup", t0))
	principal := *f.caller(member).PrincipalID
	f.absorbTypedPrompt(member, "pid-1", "tool", t0+1)
	f.hook(member, start("pid-2", "startup", t0+10))
	f.hook(member, idlePrompt("pid-1", t0+20))
	f.hook(member, idlePrompt(SessionLaunch, t0+21))
	if n := f.count(`SELECT COUNT(*) FROM observation WHERE kind = 'idle_prompt'`); n != 0 {
		t.Fatalf("%d idle facts stored for launches that are not current", n)
	}
	if n := f.openPlaceholders(principal, "pid-1"); n != 1 {
		t.Fatalf("%d open placeholders on the replaced launch, want its one", n)
	}
}

// The idle fact is ledger input, not a presence stage: the roster keeps
// showing the turn's Stop and records no change for it.
func TestIdlePromptLeavesPresenceAsItWas(t *testing.T) {
	f, _ := adapterChannel(t)
	member := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(member, start("pid-1", "startup", t0))
	f.hook(member, prompt("pid-1", "turn", t0+1))
	f.hook(member, stop("pid-1", "turn", t0+2))
	principal := *f.caller(member).PrincipalID
	latest := func() ObservedFact {
		var o *ObservedFact
		f.inTx(func(tx store.Tx) (err error) {
			o, err = latestFact(context.Background(), tx, principal)
			return err
		})
		if o == nil {
			t.Fatal("no last fact")
		}
		return *o
	}
	before := latest()
	changes := f.count(`SELECT COUNT(*) FROM roster_change`)
	f.now = time.UnixMilli(t0 + 60_000)
	f.hook(member, idlePrompt("pid-1", t0+60_000))
	if f.count(`SELECT COUNT(*) FROM observation WHERE kind = 'idle_prompt'`) != 1 {
		t.Fatal("the idle fact was not stored")
	}
	if after := latest(); after.Fact != FactTurnFinishing || after.AtMS != before.AtMS {
		t.Fatalf("last fact %+v after the idle fact, want the Stop %+v", after, before)
	}
	if n := f.count(`SELECT COUNT(*) FROM roster_change`); n != changes {
		t.Fatalf("the idle fact recorded %d roster changes", n-changes)
	}
}
