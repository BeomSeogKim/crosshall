package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// The fixture-replay harness: synthetic hook inputs, decoded into the two hook
// verbs, run against a disposable store. No real session, prompt or path.

var testProfile = wire.HookProfile{Adapter: "claude", ManifestRev: "claude-1", VendorVersion: "2.1.327",
	Platform: "darwin-arm64", Mode: "print"}

const testRepo = "/work/checkout"

func (f *fixture) policy(adapter string, enabled bool, epoch int64) {
	f.t.Helper()
	if _, err := f.db.Exec(`INSERT INTO adapter_policy (adapter, enabled, reminder, epoch, changed_at_ms)
		VALUES (?, ?, 0, ?, 0) ON CONFLICT(adapter) DO UPDATE SET enabled = excluded.enabled, epoch = excluded.epoch`,
		adapter, enabled, epoch); err != nil {
		f.t.Fatal(err)
	}
}

// hook runs a hook verb as the dispatcher would, recording invalidated stamps.
func (f *fixture) hook(s session, cmd Command) wire.HookResult {
	f.t.Helper()
	// The dispatcher's clock moves with ordinary hook times; a time far ahead
	// (a deliberate future-dated callback) does not move it.
	var at int64
	switch c := cmd.(type) {
	case HookEnroll:
		at = c.HookAtMS
	case HookEvent:
		at = c.HookAtMS
	}
	if at > f.now.UnixMilli() && at <= f.now.UnixMilli()+30_000 {
		f.now = time.UnixMilli(at)
	}
	r := f.hookResult(s, cmd)
	wantOK(f.t, r)
	return decode[wire.HookResult](f.t, r)
}

// quiet reports whether a hook result has nothing to print and nothing to acknowledge.
func quiet(r wire.HookResult) bool {
	return r.Hint == "" && r.PendingLine == "" && len(r.Roster) == 0 && r.ReminderText == "" && r.Ack == nil &&
		(r.Enrollment == "" || r.Enrollment == "none")
}

func rosterOf(t *testing.T, r wire.HookResult) []Member {
	t.Helper()
	var members []Member
	if len(r.Roster) > 0 {
		if err := json.Unmarshal(r.Roster, &members); err != nil {
			t.Fatal(err)
		}
	}
	return members
}

func (f *fixture) hookResult(s session, cmd Command) core.Result {
	c := f.caller(s)
	req := Request{Command: cmd, Vendor: s.vendor, OpKey: hookKey(cmd)}
	if c.Role == core.RoleUnenrolled {
		req.Enroll = enroll(s)
	}
	return f.exec(c, req)
}

// usable is what a manifest reports for a launch reference it can bind (a
// Claude Code PID paired at start); "session" never is.
func usable(launch string) bool {
	return launch != SessionLaunch && !strings.HasPrefix(launch, "unbound-")
}

func start(launch, source string, at int64) HookEnroll {
	return HookEnroll{wire.HookEnrollArgs{Profile: testProfile, LaunchRef: launch, LaunchVerified: usable(launch),
		EventRef: fmt.Sprintf("start:%s:%d", source, at), Source: source, RepoRoot: testRepo, HookAtMS: at, Epoch: 1}}
}

func event(launch, fact, ref, turn string, at int64, output bool) HookEvent {
	return HookEvent{wire.HookEventArgs{Profile: testProfile, LaunchRef: launch, LaunchVerified: usable(launch), Fact: fact,
		EventRef: ref, TurnRef: turn, HookAtMS: at, WantOutput: output, Epoch: 1}}
}

func prompt(launch, turn string, at int64) HookEvent {
	return event(launch, FactPromptSubmitted, turn+":prompt", turn, at, true)
}

func stop(launch, turn string, at int64) HookEvent {
	return event(launch, FactTurnFinishing, turn+":stop", turn, at, true)
}

func ended(launch string, at int64) HookEvent {
	return event(launch, FactSessionEnded, fmt.Sprintf("end:%d", at), "", at, false)
}

// adapterChannel creates #checkout with a repository default, a writer member
// (manual join) and the adapter policy on.
func adapterChannel(t *testing.T) (*fixture, session) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, "c"))
	wantOK(t, f.owner(ChannelDefault{Channel: "checkout", RepoRoot: testRepo}, "d"))
	writer := f.newSession("codex")
	wantOK(t, f.join(writer, Join{Channel: "checkout", Alias: "writer"}, ""))
	f.policy("claude", true, 1)
	return f, writer
}

func (f *fixture) noteTo(s session, alias string, n int) []int64 {
	var ids []int64
	for i := 0; i < n; i++ {
		ids = append(ids, decode[NoteResult](f.t, f.run(s, Note{To: []string{alias}, Body: fmt.Sprintf("n%d", i)}, "")).ID)
	}
	return ids
}

func pendingLine(ids ...int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return "[crosshall] pending: " + strings.Join(parts, " ")
}

// rows counts what a hook can write, to prove a replay changed nothing.
func (f *fixture) rows() map[string]int {
	out := map[string]int{}
	for _, table := range []string{"observation", "offer", "offer_item", "roster_change", "adapter_launch", "turn",
		"principal", "binding", "event", "recipient"} {
		out[table] = f.count("SELECT COUNT(*) FROM " + table)
	}
	out["live"] = f.count(`SELECT COUNT(*) FROM binding WHERE state = 'live'`)
	return out
}

func TestHookSessionReplay(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()

	inputs := []Command{
		start("pid-1", "startup", t0),
		prompt("pid-1", "turn-1", t0+1_000),
		stop("pid-1", "turn-1", t0+5_000),
		prompt("pid-1", "turn-2", t0+9_000),
		stop("pid-1", "turn-2", t0+12_000),
		ended("pid-1", t0+13_000),
		start("pid-2", "resume", t0+20_000),
		prompt("pid-2", "turn-3", t0+21_000),
	}
	var outputs []wire.HookResult
	var mail []int64
	for i, cmd := range inputs {
		if i == 3 { // mail arrives between turns
			mail = f.noteTo(writer, "claude1", 2)
		}
		outputs = append(outputs, f.hook(s, cmd))
	}
	join := outputs[0]
	if join.Enrollment != "joined" || join.Alias != "claude1" || join.Channel != "checkout" || join.Hint == "" ||
		len(rosterOf(t, join)) != 2 || join.PendingLine != "" || join.Ack == nil || join.Ack.Kind != "notice" {
		t.Fatalf("auto-join %+v", join)
	}
	if got := outputs[3].PendingLine; got != pendingLine(mail...) || outputs[3].Ack == nil || outputs[3].OfferID == 0 {
		t.Fatalf("second prompt %+v", outputs[3])
	}
	if f.count(`SELECT COUNT(*) FROM event WHERE kind = 'session_ended'`) != 1 ||
		f.count(`SELECT COUNT(*) FROM roster_change WHERE field = 'departed'`) != 1 {
		t.Fatal("a clean end of the latest launch did not depart the binding")
	}
	if outputs[6].Enrollment != "restored" || outputs[6].Alias != "claude1" || outputs[6].Hint != "" {
		t.Fatalf("restart %+v", outputs[6])
	}
	if f.count(`SELECT root_tokens FROM principal p JOIN principal_key k ON k.principal_id = p.id WHERE k.key = ?`, s.key) != 0 {
		t.Fatal("an auto-enrolled principal holds root tokens")
	}

	before := f.rows()
	for _, cmd := range inputs {
		got := f.hook(s, cmd)
		if !quiet(got) {
			t.Fatalf("replay of %T printed %+v", cmd, got)
		}
	}
	if after := f.rows(); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed rows:\n before %v\n after  %v", before, after)
	}
}

func TestElevenUnreadAreOfferedAcrossTwoPrompts(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	ids := f.noteTo(writer, "claude1", 11)

	first := f.hook(s, prompt("pid-1", "t1", t0+1_000))
	if first.PendingLine != pendingLine(ids[:10]...) || first.Ack != nil {
		t.Fatalf("first prompt %+v: ten oldest, and no acknowledgement while one remains", first)
	}
	second := f.hook(s, prompt("pid-1", "t2", t0+2_000))
	if second.PendingLine != pendingLine(ids[10]) || second.Ack == nil {
		t.Fatalf("second prompt %+v", second)
	}
	if third := f.hook(s, prompt("pid-1", "t3", t0+3_000)); third.PendingLine != "" || third.Ack == nil {
		t.Fatalf("an offered ID was offered again: %+v", third)
	}
	if n := f.count(`SELECT COUNT(*) FROM recipient WHERE picked_up_at_ms IS NOT NULL`); n != 0 {
		t.Fatalf("an offer recorded %d pickups", n)
	}
}

func TestMailDuringATurnIsOfferedAtTheNextPromptAfterStop(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, prompt("pid-1", "t1", t0+1_000))
	ids := f.noteTo(writer, "claude1", 1)
	if got := f.hook(s, stop("pid-1", "t1", t0+3_000)); got.PendingLine != "" || got.Ack != nil {
		t.Fatalf("stop evaluated notices: %+v", got)
	}
	if got := f.hook(s, prompt("pid-1", "t2", t0+4_000)); got.PendingLine != pendingLine(ids...) {
		t.Fatalf("next prompt %+v", got)
	}
}

func (f *fixture) principalOf(s session) int64 {
	c := f.caller(s)
	if c.PrincipalID == nil {
		f.t.Fatal("session has no principal")
	}
	return *c.PrincipalID
}

func TestHeldMemberGetsNoOfferAndLosesNone(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	ids := f.noteTo(writer, "claude1", 2)
	if _, err := f.db.Exec(`UPDATE binding SET dnd = 1 WHERE principal_id = ?`, f.principalOf(s)); err != nil {
		t.Fatal(err)
	}
	if got := f.hook(s, prompt("pid-1", "t1", t0+1_000)); got.PendingLine != "" || got.Ack != nil {
		t.Fatalf("held member %+v", got)
	}
	if n := f.count(`SELECT COUNT(*) FROM offer_item`); n != 0 {
		t.Fatalf("a held member spent %d offers", n)
	}
	read := decode[ReadResult](t, f.run(s, Read{IDs: ids[:1]}, ""))
	if len(read.Messages) != 1 {
		t.Fatalf("explicit read while held: %+v", read)
	}
	if _, err := f.db.Exec(`UPDATE binding SET dnd = 0`); err != nil {
		t.Fatal(err)
	}
	if got := f.hook(s, prompt("pid-1", "t2", t0+2_000)); got.PendingLine != pendingLine(ids[1]) {
		t.Fatalf("after the hold %+v", got)
	}
}

func TestSwitchedOffAdapterDoesNothing(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	for _, tc := range []struct {
		name  string
		setup func()
	}{
		{"no policy row", func() { f.db.Exec(`DELETE FROM adapter_policy`) }},
		{"switched off", func() { f.policy("claude", false, 1) }},
		{"stale epoch", func() { f.policy("claude", true, 2) }},
	} {
		tc.setup()
		before := f.rows()
		got := f.hook(s, start("pid-1", "startup", t0))
		if !quiet(got) || got.Enrollment != "" {
			t.Fatalf("%s: %+v", tc.name, got)
		}
		if after := f.rows(); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s wrote rows: %v -> %v", tc.name, before, after)
		}
	}
	// An enrolled member's events are refused the same way once the policy is off.
	f.policy("claude", true, 1)
	f.hook(s, start("pid-1", "startup", t0))
	f.policy("claude", false, 2)
	before := f.rows()
	if got := f.hook(s, prompt("pid-1", "t1", t0+1_000)); !quiet(got) {
		t.Fatalf("event after off: %+v", got)
	}
	if after := f.rows(); !reflect.DeepEqual(before, after) {
		t.Fatalf("event after off wrote rows: %v -> %v", before, after)
	}
}

func TestPresenceShowsTheLatestLiteralFactWithItsAge(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	commandAge := f.memberRow(writer, "claude1").LastCommandAgoS

	f.hook(s, prompt("pid-1", "t1", t0+1_000))
	f.now = f.now.Add(4 * time.Minute)
	m := f.memberRow(writer, "claude1")
	if m.Observed == nil || m.Observed.Fact != FactPromptSubmitted || !m.Observed.Current || m.Observed.AgeS != 240 ||
		m.Observed.Source != "claude" || m.Observed.Verified {
		t.Fatalf("observed %+v", m.Observed)
	}
	if m.LastCommandAgoS != commandAge+241 { // the prompt moved the clock 1 s, then 4 min passed
		t.Fatalf("a hook event counted as a command: %d", m.LastCommandAgoS)
	}

	f.now = f.now.Add(30 * time.Minute)
	if o := f.memberRow(writer, "claude1").Observed; o.Current || o.Fact != FactPromptSubmitted {
		t.Fatalf("past the cap %+v", o)
	}

	// A future hook time is stored but not projected; an older launch changes nothing.
	nowMS := f.now.UnixMilli()
	f.hook(s, stop("pid-1", "t1", nowMS+60_000))
	if o := f.memberRow(writer, "claude1").Observed; o.Fact != FactPromptSubmitted {
		t.Fatalf("a future hook time was projected: %+v", o)
	}
	f.hook(s, start("pid-2", "resume", nowMS))
	f.hook(s, prompt("pid-2", "t9", nowMS+1))
	f.hook(s, stop("pid-1", "t2", nowMS+2))
	if o := f.memberRow(writer, "claude1").Observed; o.Fact != FactPromptSubmitted || o.AgeS != 0 {
		t.Fatalf("an older launch changed the projection: %+v", o)
	}
}

func (f *fixture) memberRow(viewer session, alias string) Member {
	f.t.Helper()
	for _, m := range decode[WhoResult](f.t, f.who(viewer)).Members {
		if m.Alias == alias {
			return m
		}
	}
	f.t.Fatalf("@%s not in the roster", alias)
	return Member{}
}

func TestPresenceRowsOnlyWhenTheFactChanges(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	presence := func() int { return f.count(`SELECT COUNT(*) FROM roster_change WHERE field = 'presence'`) }
	f.hook(s, prompt("pid-1", "t1", t0+1_000))
	f.hook(s, event("pid-1", FactApprovalRequested, "t1:approval", "t1", t0+2_000, false))
	f.hook(s, event("pid-1", FactApprovalRequested, "t1:approval-2", "t1", t0+3_000, false))
	f.hook(s, stop("pid-1", "t1", t0+4_000))
	if n := presence(); n != 3 {
		t.Fatalf("%d presence rows, want 3 (prompt, approval, stop)", n)
	}
	f.now = f.now.Add(3 * time.Hour) // expiry alone
	f.who(s)
	if n := presence(); n != 3 {
		t.Fatalf("age wrote a presence row: %d", n)
	}
}

func TestCleanEndOfOnlyTheLatestLaunchDeparts(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, start("pid-2", "resume", t0+1_000))
	f.hook(s, ended("pid-1", t0+2_000))
	if f.memberRow(writer, "claude1").Alias == "" {
		t.Fatal("an older launch's end departed the member")
	}
	f.hook(s, event(SessionLaunch, FactSessionEnded, "end:session-scoped", "", t0+3_000, false))
	f.memberRow(writer, "claude1") // a session-scoped end of a launch never started is an observation only

	f.hook(s, ended("pid-2", t0+4_000))
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE alias = 'claude1' AND state = 'departed'`); n != 1 {
		t.Fatal("the latest launch's clean end did not depart")
	}
	// The alias is reused while it is away; it comes back under a generated one.
	other := f.newSession("claude")
	wantOK(t, f.join(other, Join{Channel: "checkout", Alias: "claude1"}, ""))
	back := f.hook(s, start("pid-3", "startup", t0+10_000))
	if back.Enrollment != "restored" || back.Alias == "claude1" || back.Alias == "" {
		t.Fatalf("restore after the alias was taken: %+v", back)
	}
}

func TestManualLeaveIsNotRestoredByAHook(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	wantOK(t, f.run(s, Leave{}, "leave"))
	if got := f.hook(s, start("pid-2", "startup", t0+1_000)); got.Enrollment != "none" {
		t.Fatalf("a member who left was restored: %+v", got)
	}
}

func TestRestoreComesBeforeTheRepositoryDefault(t *testing.T) {
	f, _ := adapterChannel(t)
	wantOK(t, f.owner(CreateChannel{Name: "inventory"}, "c2"))
	s := f.newSession("claude")
	wantOK(t, f.join(s, Join{Channel: "inventory", Alias: "stock"}, ""))
	cmd := start("pid-1", "startup", f.now.UnixMilli())
	cmd.RepoRoot = "/work/elsewhere"
	if got := f.hook(s, cmd); got.Enrollment != "restored" || got.Channel != "inventory" || got.Alias != "stock" {
		t.Fatalf("manual member outside a default repository: %+v", got)
	}
	newcomer := f.newSession("claude")
	if got := f.hook(newcomer, cmd); got.Enrollment != "none" {
		t.Fatalf("a new session joined without a default: %+v", got)
	}
	if f.caller(newcomer).Role != core.RoleUnenrolled {
		t.Fatal("a session without a default was enrolled")
	}
}

func TestCompactReoffersUnreadOncePerLaunch(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	ids := f.noteTo(writer, "claude1", 2)
	f.hook(s, prompt("pid-1", "t1", t0+1_000))
	compact := func(at int64) wire.HookResult { return f.hook(s, start("pid-1", "compact", at)) }
	if got := compact(t0 + 2_000); got.PendingLine != pendingLine(ids...) || got.Hint != "" {
		t.Fatalf("compact %+v", got)
	}
	if got := compact(t0 + 3_000); got.PendingLine != "" {
		t.Fatalf("second compact re-offered: %+v", got)
	}
}

func TestAutoEnrolledMemberBootstrapsTheRosterOnItsFirstCommand(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	wantOK(t, f.run(writer, Focus{Text: "release notes"}, "focus"))

	first := trailerOf(t, f.run(s, Inbox{}, ""))
	if !strings.Contains(first.JoinHint, "@claude1") {
		t.Fatalf("first command carries no hint: %+v", first)
	}
	if len(first.Changes) != 1 || first.Changes[0].Alias != "writer" || first.Changes[0].Change != "focus" || first.Seq == 0 {
		t.Fatalf("first command changes %+v", first)
	}
	// The CLI acknowledges after rendering; the dispatcher applies it before the next command.
	f.ack(s, first.Seq)
	next := trailerOf(t, f.exec(f.caller(s), Request{Command: Inbox{}, Ack: first.Seq}))
	if next.JoinHint != "" || len(next.Changes) != 0 {
		t.Fatalf("after the acknowledgement %+v", next)
	}
	// Later presence changes ride ordinary commands, coalesced per member.
	other := f.newSession("claude")
	f.hook(other, start("pid-9", "startup", t0+1_000))
	f.hook(other, prompt("pid-9", "a", t0+2_000))
	f.hook(other, stop("pid-9", "a", t0+3_000))
	later := trailerOf(t, f.exec(f.caller(s), Request{Command: Inbox{}}))
	var rows []string
	for _, m := range later.Changes {
		rows = append(rows, m.Alias+":"+m.Change)
	}
	if !reflect.DeepEqual(rows, []string{"claude2:presence"}) {
		t.Fatalf("changes %v", rows)
	}
}

// ack applies a CLI acknowledgement the way the dispatcher does before a handler runs.
func (f *fixture) ack(s session, seq uint64) {
	f.t.Helper()
	if _, err := f.db.Exec(`UPDATE binding SET roster_ack_seq = MAX(roster_ack_seq, ?), hint_acked = 1
		WHERE principal_id = ? AND state = 'live'`, seq, f.principalOf(s)); err != nil {
		f.t.Fatal(err)
	}
}

type memberTrailer struct {
	JoinHint string   `json:"join_hint"`
	Changes  []Member `json:"roster_changes"`
	Seq      uint64   `json:"roster_seq"`
}

func trailerOf(t *testing.T, r core.Result) memberTrailer {
	t.Helper()
	wantOK(t, r)
	return decode[memberTrailer](t, r)
}

var _ = context.Background
var _ store.Tx

func TestOwnerRosterShowsTheObservedFact(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, prompt("pid-1", "t1", t0+1_000))
	for _, ch := range decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Channels {
		for _, m := range ch.Members {
			if m.Alias == "claude1" {
				if m.Observed == nil || m.Observed.Fact != FactPromptSubmitted {
					t.Fatalf("owner roster %+v", m.Member)
				}
				return
			}
		}
	}
	t.Fatal("@claude1 missing from the owner roster")
}

func TestLeaveAfterARestoredCleanEndIsNotRestored(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, ended("pid-1", t0+1_000))
	if got := f.hook(s, start("pid-2", "startup", t0+2_000)); got.Enrollment != "restored" {
		t.Fatalf("restore after a clean end: %+v", got)
	}
	wantOK(t, f.run(s, Leave{}, "leave"))
	if got := f.hook(s, start("pid-3", "startup", t0+3_000)); got.Enrollment != "none" {
		t.Fatalf("a manual leave after an earlier clean end was restored: %+v", got)
	}
}

func TestADuplicateCallbackPrintsNothingEvenAfterNewMail(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.noteTo(writer, "claude1", 1)
	first := prompt("pid-1", "t1", t0+1_000)
	if got := f.hook(s, first); got.PendingLine == "" {
		t.Fatalf("first prompt %+v", got)
	}
	later := f.noteTo(writer, "claude1", 1)
	if got := f.hook(s, first); !quiet(got) {
		t.Fatalf("a second copy of the same prompt printed %+v", got)
	}
	if got := f.hook(s, prompt("pid-1", "t2", t0+2_000)); got.PendingLine != pendingLine(later...) {
		t.Fatalf("the next real prompt %+v", got)
	}
}

func TestASessionScopedEndIsAnObservationOnly(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("codex")
	t0 := f.now.UnixMilli()
	f.hook(s, start(SessionLaunch, "startup", t0))
	f.hook(s, ended(SessionLaunch, t0+1_000))
	m := f.memberRow(writer, "codex1")
	if m.Observed == nil || m.Observed.Fact != FactSessionEnded {
		t.Fatalf("session-scoped end %+v", m)
	}
}

func TestACleanEndDepartsOnlyWithAUsableLaunchReference(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("unbound-7", "startup", t0)) // a reference the manifest could not bind
	f.hook(s, ended("unbound-7", t0+1_000))
	if m := f.memberRow(writer, "claude1"); m.Observed == nil || m.Observed.Fact != FactSessionEnded {
		t.Fatalf("an end without a usable launch reference departed: %+v", m)
	}
}

// hookKey is the operation key the hook entry point sends.
func hookKey(cmd Command) string {
	switch c := cmd.(type) {
	case HookEnroll:
		return "hook:" + c.Profile.Adapter + ":" + c.LaunchRef + ":" + c.EventRef
	case HookEvent:
		return "hook:" + c.Profile.Adapter + ":" + c.LaunchRef + ":" + c.EventRef
	}
	return ""
}

func TestAReplayedStartDoesNotOfferNewMail(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	resume := start("pid-1", "resume", t0+1_000)
	f.hook(s, resume)
	mail := f.noteTo(writer, "claude1", 1)
	if got := f.hook(s, resume); !quiet(got) {
		t.Fatalf("a replayed start printed %+v", got)
	}
	if n := f.count(`SELECT COUNT(*) FROM offer_item`); n != 0 {
		t.Fatalf("a replayed start committed %d offers", n)
	}
	if got := f.hook(s, prompt("pid-1", "t1", t0+2_000)); got.PendingLine != pendingLine(mail...) {
		t.Fatalf("the next prompt %+v", got)
	}
}

func TestStaleCallbacksDoNotChangeTheLifecycle(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, ended("pid-1", t0+5_000))
	// A compaction delivered late, after the clean end, does not bring the member back.
	if got := f.hook(s, start("pid-1", "compact", t0+4_000)); got.Enrollment == "restored" {
		t.Fatalf("a delayed compaction resurrected a departed member: %+v", got)
	}
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE alias = 'claude1' AND state = 'live'`); n != 0 {
		t.Fatal("the departed binding is live again")
	}
	// A compaction belongs to a running session: even dated after the end it
	// does not bring the member back.
	if got := f.hook(s, start("pid-1", "compact", t0+6_000)); got.Enrollment == "restored" {
		t.Fatalf("a compaction after the end resurrected the member: %+v", got)
	}
	// A real newer start does.
	f.hook(s, start("pid-2", "startup", t0+10_000))
	// An end dated far in the future is stored but departs nobody.
	future := f.now.UnixMilli() + 60_000
	f.hook(s, ended("pid-2", future))
	if f.memberRow(writer, "claude1").Alias == "" {
		t.Fatal("a future-dated end departed the member")
	}
	// A stale prompt does not supersede a newer turn.
	f.hook(s, prompt("pid-2", "new", t0+20_000))
	f.hook(s, prompt("pid-2", "old", t0+15_000))
	if n := f.count(`SELECT COUNT(*) FROM turn WHERE turn_ref = 'new' AND superseded = 1`); n != 0 {
		t.Fatal("a stale prompt superseded the newer turn")
	}
	if n := f.count(`SELECT COUNT(*) FROM turn WHERE turn_ref = 'old' AND superseded = 0`); n != 0 {
		t.Fatal("the stale turn is current")
	}
}

func TestTheFirstWhoOfAnAutoEnrolledMemberCarriesTheBootstrap(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	f.hook(s, start("pid-1", "startup", f.now.UnixMilli()))
	got := trailerOf(t, f.who(s))
	if !strings.Contains(got.JoinHint, "@claude1") || got.Seq == 0 {
		t.Fatalf("first who %+v", got)
	}
}

func TestPresenceDeltasFollowTheEffectiveFact(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	presence := func() int { return f.count(`SELECT COUNT(*) FROM roster_change WHERE field = 'presence'`) }
	f.hook(s, prompt("pid-1", "t1", t0+1_000))
	n := presence()
	// The prompt expires; a fresh prompt makes it current again: that is a change.
	f.now = f.now.Add(31 * time.Minute)
	f.hook(s, prompt("pid-1", "t2", f.now.UnixMilli()))
	if presence() != n+1 {
		t.Fatal("an expired prompt refreshed by a new one wrote no delta")
	}
	// A stop of the same turn wins over its prompt even after a clock step back.
	at := f.now.UnixMilli() + 10_000
	f.hook(s, prompt("pid-1", "t3", at))
	f.hook(s, stop("pid-1", "t3", at-2_000))
	writer := f.memberRow(s, "claude1")
	if writer.Observed == nil || writer.Observed.Fact != FactTurnFinishing {
		t.Fatalf("same-turn order: %+v", writer.Observed)
	}
}

func TestASessionScopedLaunchStaysEligibleAcrossEndAndResume(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("codex")
	t0 := f.now.UnixMilli()
	f.hook(s, start(SessionLaunch, "startup", t0))
	f.hook(s, ended(SessionLaunch, t0+1_000))
	if n := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE ended_hook_at_ms IS NOT NULL`); n != 0 {
		t.Fatal("an unverified end ended its session-scoped launch")
	}
	f.hook(s, start(SessionLaunch, "resume", t0+2_000))
	if _, err := f.db.Exec(`DELETE FROM adapter_dirty`); err != nil { // the dispatcher published and cleared
		t.Fatal(err)
	}
	f.noteTo(writer, "codex1", 1)
	if n := f.count(`SELECT COUNT(*) FROM adapter_dirty WHERE notice = 1 AND principal_id = ?`, f.principalOf(s)); n != 1 {
		t.Fatal("mail after a resume did not invalidate the notice stamp")
	}
}

func TestANewerStartReactivatesAnEndedLaunch(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, ended("pid-1", t0+1_000))
	f.hook(s, start("pid-1", "resume", t0+2_000))
	if n := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE launch_ref = 'pid-1' AND ended_hook_at_ms IS NULL AND started_hook_at_ms = ?`, t0+2_000); n != 1 {
		t.Fatal("a newer start did not reactivate its launch")
	}
}

func TestEnrollmentProjectsCapabilitiesForTheMember(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	rows := func() int {
		return f.count(`SELECT COUNT(*) FROM capability WHERE principal_id = ? AND cap = 'enroll' AND available = 1`, f.principalOf(s))
	}
	f.hook(s, start("pid-1", "startup", t0))
	if rows() != 1 {
		t.Fatal("an auto-enrolled member has no available capability row")
	}
	f.hook(s, ended("pid-1", t0+1_000))
	if rows() != 0 {
		t.Fatal("a member that left is still shown as available")
	}
	f.hook(s, start("pid-2", "startup", t0+2_000))
	if rows() != 1 {
		t.Fatal("a restored member has no available capability row")
	}
}

func TestALaterArrivingOlderEndKeepsTheCleanEndCutoff(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, ended("pid-1", t0+3_000))
	f.hook(s, ended("pid-1", t0+2_000)) // delayed, distinct, older
	if got := f.hook(s, start("pid-1", "resume", t0+2_500)); got.Enrollment == "restored" {
		t.Fatalf("a start older than the clean end restored the member: %+v", got)
	}
}

func TestAFutureDatedStartDoesNotDisplaceTheCurrentLaunch(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, start("pid-2", "startup", t0+60_000)) // the hook's clock far ahead
	f.hook(s, ended("pid-1", t0+1_000))
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE alias = 'claude1' AND state = 'live'`); n != 0 {
		t.Fatal("a future-dated start kept the member live past its clean end")
	}
}

func TestAStalePromptOffersNothing(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, prompt("pid-1", "new", t0+20_000))
	mail := f.noteTo(writer, "claude1", 1)
	if got := f.hook(s, prompt("pid-1", "old", t0+15_000)); !quiet(got) {
		t.Fatalf("a stale prompt printed or acknowledged: %+v", got)
	}
	if got := f.hook(s, prompt("pid-1", "next", t0+25_000)); got.PendingLine != pendingLine(mail...) {
		t.Fatalf("the next real prompt %+v", got)
	}
}

func TestAResumedSessionScopedLaunchDropsItsEndedFact(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("codex")
	t0 := f.now.UnixMilli()
	f.hook(s, start(SessionLaunch, "startup", t0))
	f.hook(s, ended(SessionLaunch, t0+1_000))
	f.hook(s, start(SessionLaunch, "resume", t0+2_000))
	if o := f.memberRow(writer, "codex1").Observed; o != nil && o.Fact == FactSessionEnded {
		t.Fatalf("a resumed session still shows its end: %+v", o)
	}
}

func TestAnOlderTurnCannotReplaceANewerTurnsStop(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, prompt("pid-1", "A", t0+10_000))
	f.hook(s, prompt("pid-1", "B", t0+11_000))
	f.hook(s, stop("pid-1", "B", t0+9_000)) // the clock stepped back
	if o := f.memberRow(writer, "claude1").Observed; o == nil || o.Fact != FactTurnFinishing {
		t.Fatalf("turn A's prompt replaced turn B's stop: %+v", o)
	}
}

func TestAFutureDatedEndStaysHiddenAsTheClockCatchesUp(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, prompt("pid-1", "t1", t0+1_000))
	f.hook(s, ended("pid-1", f.now.UnixMilli()+60_000))
	f.now = f.now.Add(2 * time.Minute)
	if o := f.memberRow(writer, "claude1").Observed; o == nil || o.Fact != FactPromptSubmitted {
		t.Fatalf("a future-dated end surfaced later: %+v", o)
	}
}

func TestAFutureDatedFirstStartDoesNotAutoJoin(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	if got := f.hook(s, start("pid-9", "startup", t0+60_000)); got.Enrollment == "joined" {
		t.Fatalf("a future-dated first start auto-joined: %+v", got)
	}
	if f.caller(s).Role != core.RoleUnenrolled {
		t.Fatal("a future-dated first start enrolled the session")
	}
	if got := f.hook(s, start("pid-1", "startup", t0+1_000)); got.Enrollment != "joined" {
		t.Fatalf("the genuine start %+v", got)
	}
}

func TestASameTurnStopDatedBeforeTheStartStillWins(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, prompt("pid-1", "t1", t0+1_000))
	f.hook(s, stop("pid-1", "t1", t0-1_000)) // the clock stepped back before the start
	if o := f.memberRow(writer, "claude1").Observed; o == nil || o.Fact != FactTurnFinishing {
		t.Fatalf("the stop of the current turn was dropped: %+v", o)
	}
}
