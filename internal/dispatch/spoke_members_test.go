package dispatch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// fakeLink stands in for the SSH link: it records what the device writes
// and answers with the frames the test queues.
type fakeLink struct {
	onWrite func(frame string, v any)
	answers chan link.Frame
	written []domain.LinkCall
	frames  []string
	closed  bool
	full    bool // the pipe is full: a frame gets no byte out by its deadline
}

func newFakeLink() *fakeLink { return &fakeLink{answers: make(chan link.Frame, 8)} }

func (f *fakeLink) Send(frame string, v any, deadline time.Time) (bool, error) {
	if f.full {
		time.Sleep(time.Until(deadline))
		return false, os.ErrDeadlineExceeded
	}
	if f.onWrite != nil {
		f.onWrite(frame, v)
	}
	f.frames = append(f.frames, frame)
	f.written = append(f.written, v.(domain.LinkCall))
	return true, nil
}

func (f *fakeLink) Receive(deadline time.Time) (link.Frame, error) {
	select {
	case frame, ok := <-f.answers:
		if !ok {
			return link.Frame{}, errors.New("link ended")
		}
		return frame, nil
	case <-time.After(time.Until(deadline)):
		return link.Frame{}, os.ErrDeadlineExceeded
	}
}

func (f *fakeLink) Close() { f.closed = true }

func (f *fakeLink) answer(t *testing.T, r core.Result) {
	t.Helper()
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	f.answers <- link.Frame{Type: "result", Body: body}
}

type spokeFixture struct {
	s          *server
	m          *spokeMembers
	path       string
	dir        *os.Root
	digest     string
	generation string
}

func newSpokeFixture(t *testing.T) *spokeFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := store.OpenSpoke(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	members := filepath.Join(t.TempDir(), "rv")
	root, err := openRendezvous(members)
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	m := &spokeMembers{db: db, members: members, turn: make(chan struct{}, 1)}
	f := &spokeFixture{s: &server{spokeMembers: m}, m: m}
	var reg ident.Registration
	if err := db.Update(t.Context(), func(tx store.Tx) error {
		reg, err = ident.RegisterSpokeHello(t.Context(), tx, members,
			wire.Hello{Version: wire.Version, Attempt: wire.NewID(), Vendor: "codex", Key: "session", Intent: "join"}, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(t.Context(), func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT digest, generation FROM spoke_leaf`).Scan(&f.digest, &f.generation)
	}); err != nil {
		t.Fatal(err)
	}
	f.path = filepath.Join(members, "m", f.digest[:16])
	if f.dir, err = os.OpenRoot(f.path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.dir.Close() })
	return f
}

func (f *spokeFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.m.db.Update(t.Context(), func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), query, args...).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *spokeFixture) send(t *testing.T, verb, key string) core.Result {
	t.Helper()
	return f.sendArgs(t, verb, key, `{"all":true,"body":"hi"}`, 2*time.Second)
}

func (f *spokeFixture) sendArgs(t *testing.T, verb, key, args string, within time.Duration) core.Result {
	t.Helper()
	id := wire.NewID()
	raw, err := json.Marshal(wire.Request{Version: wire.Version, Attempt: id, OpKey: key, KeyDigest: f.digest,
		Generation: f.generation, Verb: verb, Args: json.RawMessage(args),
		Membership: &core.Membership{BindingID: 4, Generation: 2}, ExpiresAtMS: time.Now().Add(within).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.spokeCommand(t.Context(), f.path, f.dir, id, raw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A keyed command is in the outbox before its first byte reaches the
// link, and the home's answer settles it and the member's stamp.
func TestASpokeRecordsACommandBeforeSendingIt(t *testing.T) {
	f := newSpokeFixture(t)
	fake := newFakeLink()
	fake.onWrite = func(string, any) {
		if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE op_key = 'k1' AND state = 'sent'`); n != 1 {
			t.Errorf("the command reached the link before its outbox record: %d", n)
		}
	}
	f.m.attach(fake)
	fake.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{}`), Membership: &core.Membership{BindingID: 4, Generation: 3}})
	if r := f.send(t, "note", "k1"); r.Status != core.ExitOK {
		t.Fatalf("result %+v", r)
	}
	call := fake.written[0]
	if fake.frames[0] != "call" || call.Vendor != "codex" || call.Key != "session" || call.LeafGeneration != f.generation ||
		call.LeafEpoch != 1 || call.OpKey != "k1" || string(call.Args) != `{"all":true,"body":"hi"}` {
		t.Fatalf("forwarded %s %+v", fake.frames[0], call)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE state = 'done'`); n != 1 {
		t.Fatalf("%d settled records", n)
	}
	if n := f.count(t, `SELECT membership_gen FROM spoke_leaf`); n != 3 {
		t.Fatalf("membership stamp generation %d", n)
	}
}

// Nothing reaches a link that is down: the member is told nothing was
// sent, and no record is kept.
func TestASpokeWithoutALinkSendsNothing(t *testing.T) {
	f := newSpokeFixture(t)
	if r := f.send(t, "note", "k1"); r.Code != core.CodeUnsent {
		t.Fatalf("result %+v", r)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox`); n != 0 {
		t.Fatalf("%d records for an unsent command", n)
	}
}

// A link that ends before its answer leaves the outcome unknown and the
// link closed; the next connection looks the record up receipt-only, as
// it does for a record a crash left as sent.
func TestASpokeLooksUpALostAnswerReceiptOnly(t *testing.T) {
	f := newSpokeFixture(t)
	ended := newFakeLink()
	close(ended.answers)
	f.m.attach(ended)
	r := f.send(t, "note", "k1")
	if r.Code != core.CodeOutcomeUnknown || !ended.closed || f.m.conn != nil {
		t.Fatalf("result %+v, closed %v", r, ended.closed)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE state = 'unknown'`); n != 1 {
		t.Fatalf("%d unknown records", n)
	}
	// A crash after the record and before the answer.
	if err := f.m.db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO link_outbox (vendor, key, op_key, command, args, state, at_ms)
			VALUES ('codex', 'session', 'k2', 'join', '{"channel":"x"}', 'sent', 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	next := newFakeLink()
	fenced := domain.Refuse(domain.CodeLinkFenced, "unknown", "run it again")
	next.answer(t, fenced)
	next.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{}`)})
	if !f.s.recoverOutbox(t.Context(), next) {
		t.Fatal("recovery failed")
	}
	if len(next.frames) != 2 || next.frames[0] != "receipt" || next.frames[1] != "receipt" ||
		next.written[0].OpKey != "k1" || string(next.written[0].Args) != `{"all":true,"body":"hi"}` ||
		next.written[0].Membership == nil || next.written[0].Membership.Generation != 2 ||
		next.written[1].OpKey != "k2" || next.written[1].Command != "join" {
		t.Fatalf("lookups %v %+v", next.frames, next.written)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE state = 'done'`); n != 2 {
		t.Fatalf("%d settled records", n)
	}
	var stored string
	if err := f.m.db.Update(t.Context(), func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT result FROM link_outbox WHERE op_key = 'k1'`).Scan(&stored)
	}); err != nil || !json.Valid([]byte(stored)) {
		t.Fatalf("stored %q, %v", stored, err)
	}
	var got core.Result
	if json.Unmarshal([]byte(stored), &got); got.Code != domain.CodeLinkFenced {
		t.Fatalf("the fenced answer was not kept: %s", stored)
	}
}

// A home that closed the link (link_stale) closes it on the device too,
// so the reconnect loop dials again.
func TestASpokeClosesALinkTheHomeClosed(t *testing.T) {
	f := newSpokeFixture(t)
	fake := newFakeLink()
	f.m.attach(fake)
	fake.answer(t, domain.Refuse(domain.CodeLinkStale, "stale", "reconnect the link"))
	if r := f.send(t, "who", ""); r.Code != core.CodeOutcomeUnknown || !fake.closed || f.m.conn != nil {
		t.Fatalf("result %+v, closed %v", r, fake.closed)
	}
}

// crosshall retry shows an outcome the home answered, looks a lost one up
// first, and sends again only what the home fenced, under its own new key
// with the original bytes.
func TestASpokeRetrySendsAgainOnlyWhatNeverRan(t *testing.T) {
	f := newSpokeFixture(t)
	// k1: answered. k2: answer lost, and the home has it. k3: answer lost,
	// and the home fences it.
	fake := newFakeLink()
	f.m.attach(fake)
	fake.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{"id":1}`)})
	f.send(t, "note", "k1")
	for _, key := range []string{"k2", "k3"} {
		ended := newFakeLink()
		close(ended.answers)
		f.m.attach(ended)
		if r := f.send(t, "note", key); r.Code != core.CodeOutcomeUnknown {
			t.Fatalf("%s: %+v", key, r)
		}
	}
	link := newFakeLink()
	f.m.attach(link)
	retry := func(key, newKey string) core.Result {
		t.Helper()
		return f.sendArgs(t, domain.VerbRetry, newKey, `{"key":"`+key+`"}`, 2*time.Second)
	}
	inner := func(r core.Result) string {
		t.Helper()
		var out domain.RetryResult
		if err := json.Unmarshal(r.Data, &out); err != nil || out.Command != "note" {
			t.Fatalf("retry result %s", r.Data)
		}
		return string(out.Result)
	}
	if r := retry("k1", "r1"); r.Status != core.ExitOK || inner(r) != `{"id":1}` || len(link.frames) != 0 {
		t.Fatalf("answered retry %+v, frames %v", r, link.frames)
	}
	link.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{"id":2}`)})
	if r := retry("k2", "r2"); r.Status != core.ExitOK || inner(r) != `{"id":2}` ||
		len(link.frames) != 1 || link.frames[0] != "receipt" || link.written[0].OpKey != "k2" {
		t.Fatalf("lost retry %+v, frames %v", r, link.frames)
	}
	link.answer(t, domain.Refuse(domain.CodeLinkFenced, "unknown", "run it again"))
	link.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{"id":3}`)})
	if r := retry("k3", "r3"); r.Status != core.ExitOK || inner(r) != `{"id":3}` {
		t.Fatalf("fenced retry %+v", r)
	}
	resend := link.written[2]
	if link.frames[1] != "receipt" || link.frames[2] != "call" || resend.OpKey != "r3" || resend.Command != "note" ||
		string(resend.Args) != `{"all":true,"body":"hi"}` {
		t.Fatalf("resend %v %+v", link.frames, resend)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE op_key = 'r3' AND state = 'done'`); n != 1 {
		t.Fatalf("the resend has no record of its own")
	}
	if r := retry("k9", "r9"); r.Code != core.CodeNotFound {
		t.Fatalf("unknown key %+v", r)
	}
}

// A key already in the outbox is never reset or dropped: with the link
// down the record stays for recovery, other bytes are a conflict, and the
// same request is looked up, never sent again.
func TestASpokeNeverResetsAKeyInItsOutbox(t *testing.T) {
	f := newSpokeFixture(t)
	ended := newFakeLink()
	close(ended.answers)
	f.m.attach(ended)
	f.send(t, "note", "k1")
	f.m.attach(nil)
	if r := f.send(t, "note", "k1"); r.Code != core.CodeUnsent {
		t.Fatalf("same key, link down: %+v", r)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE op_key = 'k1' AND state = 'unknown'`); n != 1 {
		t.Fatalf("the unresolved record is gone or changed")
	}
	link := newFakeLink()
	f.m.attach(link)
	if r := f.sendArgs(t, "note", "k1", `{"all":true,"body":"other"}`, 2*time.Second); r.Code != core.CodeKeyConflict || len(link.frames) != 0 {
		t.Fatalf("other bytes under the key: %+v, frames %v", r, link.frames)
	}
	link.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{"id":7}`)})
	if r := f.send(t, "note", "k1"); r.Status != core.ExitOK || len(link.frames) != 1 || link.frames[0] != "receipt" {
		t.Fatalf("same request: %+v, frames %v", r, link.frames)
	}
	if r := f.send(t, "note", "k1"); string(r.Data) != `{"id":7}` || len(link.frames) != 1 {
		t.Fatalf("answered request: %+v, frames %v", r, link.frames)
	}
}

// Recovery runs on a new link before it is published: a fresh command
// meanwhile sends nothing and leaves no record, so recovery never fences
// it; once recovery is done, the link carries fresh commands.
func TestASpokeRecoversBeforeItSendsFreshCommands(t *testing.T) {
	f := newSpokeFixture(t)
	next := newFakeLink()
	release := make(chan struct{})
	next.onWrite = func(string, any) { <-release }
	next.answer(t, domain.Refuse(domain.CodeLinkFenced, "unknown", "run it again"))
	if err := f.m.db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO link_outbox (vendor, key, op_key, command, args, state, at_ms)
			VALUES ('codex', 'session', 'old', 'note', '{}', 'unknown', 1)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool)
	go func() { done <- f.s.recoverOutbox(t.Context(), next) }()
	for len(f.m.turn) == 0 {
		time.Sleep(time.Millisecond)
	}
	if r := f.sendArgs(t, "note", "fresh", `{"all":true,"body":"hi"}`, 50*time.Millisecond); r.Code != core.CodeUnsent {
		t.Fatalf("a fresh command during recovery: %+v", r)
	}
	close(release)
	if !<-done {
		t.Fatal("recovery failed")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE op_key = 'fresh'`); n != 0 || len(next.frames) != 1 {
		t.Fatalf("recovery saw a fresh record: %d, frames %v", n, next.frames)
	}
	next.onWrite = nil
	f.m.attach(next)
	next.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{}`)})
	if r := f.send(t, "note", "fresh"); r.Status != core.ExitOK || next.frames[1] != "call" {
		t.Fatalf("after recovery: %+v, frames %v", r, next.frames)
	}
}

// A join's launch, which a member client adds on its own, names a process
// on this device: it is not forwarded. Nothing is written once the
// command's deadline has passed waiting for the link.
func TestASpokeForwardsAJoinWithoutItsLocalLaunch(t *testing.T) {
	f := newSpokeFixture(t)
	link := newFakeLink()
	f.m.attach(link)
	link.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{}`)})
	f.sendArgs(t, "join", "j1", `{"channel":"checkout","launch":{"ref":"x"}}`, 2*time.Second)
	if got := string(link.written[0].Args); got != `{"channel":"checkout"}` {
		t.Fatalf("forwarded join %s", got)
	}
	f.m.turn <- struct{}{} // another exchange holds the link past the deadline
	if r := f.sendArgs(t, "note", "late", `{"all":true,"body":"hi"}`, 30*time.Millisecond); r.Code != core.CodeUnsent || len(link.frames) != 1 {
		t.Fatalf("an expired command: %+v, frames %v", r, link.frames)
	}
	<-f.m.turn
}

// A command whose deadline passes while it waits to journal, or before
// its first byte, sends nothing and leaves no record of its own.
func TestASpokeSendsNothingPastItsDeadline(t *testing.T) {
	f := newSpokeFixture(t)
	link := newFakeLink()
	f.m.attach(link)
	held := make(chan struct{})
	go f.m.db.Update(t.Context(), func(store.Tx) error {
		close(held)
		time.Sleep(200 * time.Millisecond) // another writer holds the store
		return nil
	})
	<-held
	started := time.Now()
	if r := f.sendArgs(t, "note", "slow", `{"all":true,"body":"hi"}`, 50*time.Millisecond); r.Code != core.CodeUnsent {
		t.Fatalf("journaling past the deadline: %+v", r)
	}
	if waited := time.Since(started); waited > 150*time.Millisecond {
		t.Fatalf("the command waited %v for the store, past its deadline", waited)
	}
	if _, err := f.m.roundTrip(t.Context(), link, "call", domain.LinkCall{OpKey: "late"}, time.Now().Add(-time.Millisecond)); !errors.Is(err, errNotSent) {
		t.Fatalf("a round trip past its deadline: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox`); n != 0 || len(link.frames) != 0 || link.closed {
		t.Fatalf("%d records, frames %v, closed %v", n, link.frames, link.closed)
	}
}

// A fenced command is sent again once: a replay of the same retry, a
// replay with the link down, and a repeated retry under another key all
// resolve that resend, receipt-only, and never send another.
func TestASpokeRetrySendsAFencedCommandAgainOnce(t *testing.T) {
	f := newSpokeFixture(t)
	ended := newFakeLink()
	close(ended.answers)
	f.m.attach(ended)
	f.send(t, "note", "k1")
	retry := func(newKey string) core.Result {
		t.Helper()
		return f.sendArgs(t, domain.VerbRetry, newKey, `{"key":"k1"}`, 2*time.Second)
	}
	// The resend's answer is lost too.
	lossy := newFakeLink()
	lossy.answer(t, domain.Refuse(domain.CodeLinkFenced, "unknown", "run it again"))
	lossy.onWrite = func(frame string, _ any) {
		if frame == "call" {
			close(lossy.answers) // the link ends before the resend's answer
		}
	}
	f.m.attach(lossy)
	if r := retry("r1"); r.Code != core.CodeOutcomeUnknown || len(lossy.frames) != 2 || lossy.frames[1] != "call" || lossy.written[1].OpKey != "r1" {
		t.Fatalf("first retry %+v, frames %v", r, lossy.frames)
	}
	f.m.attach(nil)
	if r := retry("r1"); r.Code != core.CodeUnsent {
		t.Fatalf("replay with the link down %+v", r)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE op_key = 'r1'`); n != 1 {
		t.Fatalf("the resend's record is gone")
	}
	link := newFakeLink()
	f.m.attach(link)
	// k1's fenced answer is stored: only the resend is looked up.
	link.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{"id":5}`)})
	if r := retry("r2"); r.Status != core.ExitOK || len(link.frames) != 1 || link.frames[0] != "receipt" || link.written[0].OpKey != "r1" {
		t.Fatalf("repeated retry %+v, frames %v %+v", r, link.frames, link.written)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE op_key = 'r2'`); n != 0 {
		t.Fatalf("a repeated retry recorded another resend")
	}
}

// A recovered answer never rolls the member's selection back.
func TestASpokeKeepsTheNewestMembership(t *testing.T) {
	f := newSpokeFixture(t)
	link := newFakeLink()
	f.m.attach(link)
	link.answer(t, core.Result{Status: core.ExitOK, Data: json.RawMessage(`{}`), Membership: &core.Membership{BindingID: 7, Generation: 3}})
	f.send(t, "note", "k1")
	f.s.adoptMembership(t.Context(), "codex", "session", &core.Membership{BindingID: 4, Generation: 2})
	if n := f.count(t, `SELECT binding_id * 10 + membership_gen FROM spoke_leaf`); n != 73 {
		t.Fatalf("selection %d after an older receipt", n)
	}
}

// A frame that gets no byte out by its deadline (the home stopped
// reading) leaves nothing behind: unsent, the link closed, and the
// command's own record removed. A home that reads but never answers
// leaves the outcome unknown.
func TestASpokeTellsUnsentFromUnanswered(t *testing.T) {
	f := newSpokeFixture(t)
	full := newFakeLink()
	full.full = true
	f.m.attach(full)
	if r := f.sendArgs(t, "note", "k1", `{"all":true,"body":"hi"}`, 50*time.Millisecond); r.Code != core.CodeUnsent || !full.closed {
		t.Fatalf("a full pipe: %+v, closed %v", r, full.closed)
	}
	silent := newFakeLink()
	f.m.attach(silent)
	if r := f.sendArgs(t, "note", "k2", `{"all":true,"body":"hi"}`, 50*time.Millisecond); r.Code != core.CodeOutcomeUnknown || !silent.closed {
		t.Fatalf("a silent home: %+v", r)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE op_key = 'k1'`); n != 0 {
		t.Fatalf("an unsent command kept its record")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM link_outbox WHERE op_key = 'k2' AND state = 'unknown'`); n != 1 {
		t.Fatalf("an unanswered command lost its record")
	}
}
