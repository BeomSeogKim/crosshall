package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// linkServer is a home dispatcher with two linked devices and two
// channels. Every OS probe fails the test: a forwarded command never
// looks at a process on the home device.
func linkServer(t *testing.T) *server {
	t.Helper()
	s := reviewServer(t)
	probe := func(name string) { t.Errorf("a link call probed %s on the home device", name) }
	s.cfg.Handle = &domain.Service{
		LaunchAlive:        func(string, int64) bool { probe("LaunchAlive"); return false },
		LaunchProcess:      func(string) (string, bool) { probe("LaunchProcess"); return "", false },
		LaunchPIDNamespace: func() string { probe("LaunchPIDNamespace"); return "" },
		ProcessGone:        func(int64, int64) bool { probe("ProcessGone"); return false },
		ProcessAlive:       func(int64, int64) bool { probe("ProcessAlive"); return false },
		HasPromptLine:      func(string, string) bool { probe("HasPromptLine"); return false },
		HasSessionEnd:      func(string, string) bool { probe("HasSessionEnd"); return false },
		LeafReceipts: func(string, string, string, string, ...core.Membership) (uint64, uint64) {
			probe("LeafReceipts")
			return 0, 0
		},
	}
	s.fixtureSQL(t, `INSERT INTO principal (kind, vendor, created_at_ms) VALUES ('owner', 'owner', 1);
		INSERT INTO channel (name, created_at_ms) VALUES ('checkout', 1), ('payments', 1);
		INSERT INTO device (device_id, name, secret, created_at_ms, secret_at_ms) VALUES
			('dev-a', 'macbook', x'00', 1, 1), ('dev-b', 'laptop', x'00', 1, 1);`)
	return s
}

type linkReq struct {
	device, key, gen string
	cred, epoch      int64
	command          string
	args             any
	op               string
	membership       *core.Membership
	ack              uint64
	receipt          bool
	admission        int64
}

func (s *server) link(t *testing.T, r linkReq) core.Result {
	t.Helper()
	if r.device == "" {
		r.device = "dev-a"
	}
	if r.key == "" {
		r.key = "k1"
	}
	if r.gen == "" {
		r.gen = "g1"
	}
	if r.cred == 0 {
		r.cred = 1
	}
	if r.epoch == 0 {
		r.epoch = 1
	}
	if r.admission == 0 {
		r.admission = 1 // a device's first admission generation
	}
	args, err := json.Marshal(r.args)
	if err != nil {
		t.Fatal(err)
	}
	call, err := json.Marshal(domain.LinkCall{Device: r.device, CredentialGen: r.cred, Admission: r.admission, Vendor: "claude", Key: r.key,
		LeafGeneration: r.gen, LeafEpoch: r.epoch, Command: r.command, Args: args, OpKey: r.op, Membership: r.membership, Ack: r.ack})
	if err != nil {
		t.Fatal(err)
	}
	verb := domain.VerbLinkCall
	if r.receipt {
		verb = domain.VerbLinkReceipt
	}
	return s.fixtureRequest(t, Arrival{Owner: true}, wire.Request{Verb: verb, Args: call})
}

func (s *server) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), query, args...).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustLinkOK(t *testing.T, r core.Result) core.Result {
	t.Helper()
	if r.Status != core.ExitOK {
		t.Fatalf("status %d code %s data %s", r.Status, r.Code, r.Data)
	}
	return r
}

func mustLinkCode(t *testing.T, r core.Result, code core.Code) {
	t.Helper()
	if r.Code != code {
		t.Fatalf("code %q (status %d, %s), want %q", r.Code, r.Status, r.Data, code)
	}
}

func joinChannel(name string) map[string]any { return map[string]any{"channel": name} }

func TestALinkedDeviceMemberJoinsAndIsKeyedByDevice(t *testing.T) {
	s := linkServer(t)
	r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}))
	if r.Membership == nil || r.Membership.BindingID == 0 {
		t.Fatalf("join returned no membership to stamp: %+v", r)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM device_key k JOIN principal p ON p.id = k.principal_id
		WHERE k.device_id = 'dev-a' AND p.device_id = 'dev-a' AND k.leaf_epoch = 1`); n != 1 {
		t.Fatalf("%d device keys", n)
	}
	// No home key, leaf, launch or route for a device member.
	if n := s.count(t, `SELECT (SELECT COUNT(*) FROM principal_key) + (SELECT COUNT(*) FROM leaf)
		+ (SELECT COUNT(*) FROM adapter_launch) + (SELECT COUNT(*) FROM publish_route)`); n != 0 {
		t.Fatalf("%d home-local rows", n)
	}
	// The same vendor and key on another device is another member.
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", cred: 1, command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}))
	if n := s.count(t, `SELECT COUNT(DISTINCT principal_id) FROM device_key WHERE key = 'k1'`); n != 2 {
		t.Fatalf("%d principals for one key on two devices", n)
	}
	// A repeated first join of one tuple resolves to the same member.
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j2", membership: r.Membership}))
	if n := s.count(t, `SELECT COUNT(*) FROM device_key WHERE device_id = 'dev-a'`); n != 1 {
		t.Fatalf("%d keys on dev-a", n)
	}
}

func TestALinkCallUnderAnOldCredentialIsRefusedBeforeLookup(t *testing.T) {
	s := linkServer(t)
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}))
	s.fixtureSQL(t, `UPDATE device SET credential_gen = 2 WHERE device_id = 'dev-a'`)
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, cred: 1}), domain.CodeLinkStale)
	mustLinkCode(t, s.link(t, linkReq{device: "dev-x", command: domain.VerbWho}), domain.CodeLinkStale)
	// A link authenticated under the new credential resumes the device,
	// and its leaf counter may start again.
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWho, cred: 2, epoch: 1, gen: "g9"}))
}

func TestStaleLeafRegistrationsAreRefused(t *testing.T) {
	s := linkServer(t)
	r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1", epoch: 3, gen: "g3"}))
	m := r.Membership
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 3, gen: "other", membership: m}), domain.CodeStaleLeaf)
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 2, gen: "g2", membership: m}), domain.CodeStaleLeaf)
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 3, gen: "g3", membership: m}))
	// A newer registration replaces it; the older one is then stale.
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 4, gen: "g4", membership: m}))
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 3, gen: "g3", membership: m}), domain.CodeStaleLeaf)
}

// A forwarded command carries only what the hall can act on: nothing
// about the device's processes, paths or other sessions.
func TestLinkCommandsHaveALinkShape(t *testing.T) {
	s := linkServer(t)
	for name, c := range map[string]struct {
		command string
		args    any
		code    core.Code
	}{
		"launch":        {domain.VerbJoin, map[string]any{"channel": "checkout", "launch": map[string]any{"ref": "x"}}, domain.CodeNotOnADevice},
		"takeover":      {domain.VerbJoin, map[string]any{"channel": "checkout", "alias": "a", "takeover": true}, domain.CodeNotOnADevice},
		"repo root":     {domain.VerbJoin, map[string]any{"channel": "checkout", "repo_root": "/Users/x/repo"}, domain.CodeNotOnADevice},
		"no channel":    {domain.VerbJoin, map[string]any{}, ""},
		"unknown field": {domain.VerbJoin, map[string]any{"channel": "checkout", "host": 1}, ""},
		"exit tracking": {domain.VerbWho, map[string]any{"exit_tracking": true}, domain.CodeNotOnADevice},
		"hook verb":     {domain.VerbHookEvent, map[string]any{}, domain.CodeNotOnADevice},
		"owner verb":    {domain.VerbCreateChannel, map[string]any{"name": "x"}, domain.CodeNotOnADevice},
	} {
		r := s.link(t, linkReq{command: c.command, args: c.args, op: "op-" + name})
		if r.Status == core.ExitOK || c.code != "" && r.Code != c.code {
			t.Errorf("%s: status %d code %q", name, r.Status, r.Code)
		}
	}
	if n := s.count(t, `SELECT (SELECT COUNT(*) FROM device_key) + (SELECT COUNT(*) FROM principal WHERE kind = 'session')
		+ (SELECT COUNT(*) FROM channel WHERE name = 'x')`); n != 0 {
		t.Fatalf("%d rows written by refused forms", n)
	}
}

func TestAPlainJoinElsewhereIsRefusedAndAMoveIsExplicit(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	r := s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("payments"), op: "j2", membership: m})
	if r.Status == core.ExitOK {
		t.Fatalf("a plain join moved the member: %+v", r)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM member_selection s JOIN binding b ON b.id = s.binding_id
		JOIN channel c ON c.id = b.channel_id WHERE c.name = 'checkout'`); n != 1 {
		t.Fatalf("the source selection changed: %d", n)
	}
	moved := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: map[string]any{"channel": "payments", "move": true},
		op: "j3", membership: m})).Membership
	if moved == nil || moved.BindingID == m.BindingID {
		t.Fatalf("move membership %+v, before %+v", moved, m)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM member_selection s JOIN binding b ON b.id = s.binding_id
		JOIN channel c ON c.id = b.channel_id WHERE c.name = 'payments'`); n != 1 {
		t.Fatalf("the move did not select #payments: %d", n)
	}
}

// An ask whose response was lost, then a move, then the retry: the retry
// gets the original receipt and asks nothing again. A retry of an ask
// that never committed is refused once the member moved.
func TestARetryAfterAMoveGetsTheCommittedReceiptOnly(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"}))
	ask := map[string]any{"all": true, "each": true, "body": "status?"}
	first := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbAsk, args: ask, op: "a1", membership: m}))
	moved := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: map[string]any{"channel": "payments", "move": true},
		op: "j2", membership: m})).Membership
	again := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbAsk, args: ask, op: "a1", membership: m}))
	// The stored receipt is the original ask; replay adds no live fields.
	var a, b struct {
		ID      int64  `json:"id"`
		Channel string `json:"channel"`
	}
	if json.Unmarshal(first.Data, &a) != nil || json.Unmarshal(again.Data, &b) != nil || a != b || a.ID == 0 || b.Channel != "checkout" {
		t.Fatalf("retry %s, first %s", again.Data, first.Data)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM message WHERE body = 'status?'`); n != 1 {
		t.Fatalf("%d asks stored", n)
	}
	// The same key for another command, or under another membership, is
	// a conflict, not a replay.
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbAsk, args: map[string]any{"all": true, "each": true, "body": "other"}, op: "a1", membership: moved}),
		core.CodeKeyConflict)
	// Never committed: admission refuses it under the old membership.
	r := s.link(t, linkReq{command: domain.VerbAsk, args: map[string]any{"all": true, "each": true, "body": "late"}, op: "a2", membership: m})
	mustLinkCode(t, r, core.CodeBindingChanged)
	if n := s.count(t, `SELECT COUNT(*) FROM message WHERE body = 'late'`); n != 0 {
		t.Fatalf("%d late asks stored", n)
	}
}

func TestOnlyTheOwnerIngressCarriesALinkCall(t *testing.T) {
	s := linkServer(t)
	call, _ := json.Marshal(domain.LinkCall{Device: "dev-a", CredentialGen: 1, Vendor: "claude", Key: "k1",
		LeafGeneration: "g1", LeafEpoch: 1, Command: domain.VerbWho})
	r := s.fixtureRequest(t, Arrival{}, wire.Request{Verb: domain.VerbLinkCall, Args: call})
	if r.Status == core.ExitOK {
		t.Fatalf("a member ingress ran a link call: %+v", r)
	}
}

// A read's receipt is shown again with its bodies, against the channel it
// was read in, also after a move and with duplicate IDs in the request;
// the retry records no new pickup.
func TestARetriedLinkedReadShowsTheOriginalMessages(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"}))
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbNote, args: map[string]any{"all": true, "body": "hello there"}, op: "n1"}))
	body := func(r core.Result) string {
		var out domain.ReadResult
		if err := json.Unmarshal(r.Data, &out); err != nil || len(out.Messages) != 1 {
			t.Fatalf("read %s", r.Data)
		}
		return out.Messages[0].Body
	}
	read := map[string]any{"ids": []int64{1, 1}}
	if b := body(mustLinkOK(t, s.link(t, linkReq{command: domain.VerbRead, args: read, op: "r1", membership: m}))); b != "hello there" {
		t.Fatalf("first read %q", b)
	}
	if b := body(mustLinkOK(t, s.link(t, linkReq{command: domain.VerbRead, args: read, op: "r1", membership: m}))); b != "hello there" {
		t.Fatalf("exact retry %q", b)
	}
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: map[string]any{"channel": "payments", "move": true}, op: "j2", membership: m}))
	if b := body(mustLinkOK(t, s.link(t, linkReq{command: domain.VerbRead, args: read, op: "r1", membership: m}))); b != "hello there" {
		t.Fatalf("retry after a move %q", b)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM op WHERE op_key = 'r1'`); n != 1 {
		t.Fatalf("%d read ops", n)
	}
}

// A read whose answer would not fit one link frame is refused and leaves
// nothing picked up.
func TestALinkedReadTooLargeForTheLinkPicksNothingUp(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"}))
	// "<" is written as \u003c in JSON: six bytes for one.
	big := strings.Repeat("<", domain.MaxBody)
	for i := range 3 {
		mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbNote,
			args: map[string]any{"all": true, "body": big}, op: fmt.Sprintf("n%d", i)}))
	}
	r := s.link(t, linkReq{command: domain.VerbRead, args: map[string]any{}, op: "r1", membership: m})
	mustLinkCode(t, r, domain.CodeLinkResultTooLarge)
	if n := s.count(t, `SELECT COUNT(*) FROM recipient r JOIN principal p ON p.id = r.principal_id
		WHERE p.device_id = 'dev-a' AND r.picked_up_at_ms IS NOT NULL`); n != 0 {
		t.Fatalf("%d messages picked up by a refused read", n)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM op WHERE op_key = 'r1'`); n != 0 {
		t.Fatalf("the refused read recorded its key")
	}
	// One message at a time fits.
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbRead, args: map[string]any{"ids": []int64{1}}, op: "r2", membership: m}))
}

// A newer leaf registration is kept whatever its command's outcome; the
// older registration is stale afterwards.
func TestALeafReplacementHoldsWhateverTheCommandsOutcome(t *testing.T) {
	for name, second := range map[string]linkReq{
		"refused command": {command: domain.VerbFocus, args: map[string]any{}, op: "f1"},
		"receipt replay":  {command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"},
	} {
		t.Run(name, func(t *testing.T) {
			s := linkServer(t)
			m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
			second.epoch, second.gen = 2, "g2"
			if second.command != domain.VerbJoin {
				second.membership = m
			}
			s.link(t, second)
			if n := s.count(t, `SELECT leaf_epoch FROM device_key WHERE device_id = 'dev-a'`); n != 2 {
				t.Fatalf("leaf epoch %d after a newer registration", n)
			}
			mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, membership: m}), domain.CodeStaleLeaf)
		})
	}
}

// After a linked leave, a join with the member's earlier roster cursor
// brings it back.
func TestALinkedMemberRejoinsAfterLeaving(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"}))
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWho, membership: m}))
	left := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbLeave, op: "l1", membership: m, ack: 1}))
	membership := left.Membership
	if membership == nil {
		membership = m
	}
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j2", membership: membership, ack: 1}))
}

// An unknown key joins only with an empty selection; a refused first join
// writes nothing.
func TestAFirstLinkedJoinMustCarryNoSelection(t *testing.T) {
	s := linkServer(t)
	other := mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"})).Membership
	before := s.count(t, `SELECT (SELECT COUNT(*) FROM principal) * 1000 + (SELECT COUNT(*) FROM binding) * 100
		+ (SELECT COUNT(*) FROM device_key) * 10 + (SELECT COUNT(*) FROM op)`)
	mustLinkCode(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1", membership: other}),
		core.CodeBindingChanged)
	after := s.count(t, `SELECT (SELECT COUNT(*) FROM principal) * 1000 + (SELECT COUNT(*) FROM binding) * 100
		+ (SELECT COUNT(*) FROM device_key) * 10 + (SELECT COUNT(*) FROM op)`)
	if after != before {
		t.Fatalf("a refused first join wrote rows: %d -> %d", before, after)
	}
}

// A queued move without the selection it was issued under is refused, as
// at native ingress: the source stays selected and no op is recorded.
func TestALinkedMoveNeedsItsSelection(t *testing.T) {
	s := linkServer(t)
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}))
	r := s.link(t, linkReq{command: domain.VerbJoin, args: map[string]any{"channel": "payments", "move": true}, op: "j2"})
	mustLinkCode(t, r, core.CodeBindingChanged)
	if n := s.count(t, `SELECT COUNT(*) FROM member_selection s JOIN binding b ON b.id = s.binding_id
		JOIN channel c ON c.id = b.channel_id WHERE c.name = 'checkout'`); n != 1 {
		t.Fatalf("the source is no longer selected: %d", n)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM op WHERE op_key = 'j2'`); n != 0 {
		t.Fatalf("the refused move recorded its op")
	}
	// A plain rejoin of the same channel needs no selection.
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j3"}))
}

// A lookup returns the committed receipt of a forwarded request, and runs
// nothing.
func TestALinkReceiptReturnsTheCommittedReceipt(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"}))
	ask := linkReq{command: domain.VerbAsk, args: map[string]any{"all": true, "each": true, "body": "hi"}, op: "a1", membership: m}
	first := mustLinkOK(t, s.link(t, ask))
	ask.receipt, ask.epoch = true, 99
	again := mustLinkOK(t, s.link(t, ask))
	var a, b struct {
		ID      int64  `json:"id"`
		Channel string `json:"channel"`
	}
	if json.Unmarshal(first.Data, &a) != nil || json.Unmarshal(again.Data, &b) != nil || a != b || a.ID == 0 {
		t.Fatalf("receipt %s, original %s", again.Data, first.Data)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM message`); n != 1 {
		t.Fatalf("%d messages after a lookup", n)
	}
	if n := s.count(t, `SELECT leaf_epoch FROM device_key WHERE device_id = 'dev-a'`); n != 1 {
		t.Fatalf("a lookup moved the leaf epoch to %d", n)
	}
	other := ask
	other.args = map[string]any{"all": true, "each": true, "body": "something else"}
	mustLinkCode(t, s.link(t, other), core.CodeKeyConflict)
}

// A lookup that finds nothing fences the key: the request, still queued,
// never runs afterwards, and a fresh key sends it once.
func TestALinkReceiptFencesARequestItCannotFind(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"}))
	note := linkReq{command: domain.VerbNote, args: map[string]any{"all": true, "body": "hello"}, op: "n1", membership: m}
	lookup := note
	lookup.receipt = true
	mustLinkCode(t, s.link(t, lookup), domain.CodeLinkFenced)
	mustLinkCode(t, s.link(t, note), domain.CodeLinkFenced)
	mustLinkCode(t, s.link(t, lookup), domain.CodeLinkFenced)
	if n := s.count(t, `SELECT COUNT(*) FROM message`); n != 0 {
		t.Fatalf("the fenced note ran: %d messages", n)
	}
	note.op = "n2"
	mustLinkOK(t, s.link(t, note))
	if n := s.count(t, `SELECT COUNT(*) FROM message`); n != 1 {
		t.Fatalf("%d messages after a fresh send", n)
	}
}

// A fence written before the session was known holds after the session
// joins under another key, and enrolls nothing.
func TestALinkReceiptFencesAFirstJoin(t *testing.T) {
	s := linkServer(t)
	join := linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}
	lookup := join
	lookup.receipt = true
	mustLinkCode(t, s.link(t, lookup), domain.CodeLinkFenced)
	mustLinkCode(t, s.link(t, join), domain.CodeLinkFenced)
	if n := s.count(t, `SELECT COUNT(*) FROM device_key`); n != 0 {
		t.Fatalf("a fenced join enrolled %d keys", n)
	}
	other := join
	other.args = joinChannel("payments")
	mustLinkCode(t, s.link(t, other), core.CodeKeyConflict)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j2"})).Membership
	if m == nil {
		t.Fatal("no membership after the fresh join")
	}
	mustLinkCode(t, s.link(t, join), domain.CodeLinkFenced)
	mustLinkCode(t, s.link(t, lookup), domain.CodeLinkFenced)
}

// A lookup under an old credential is refused and writes nothing.
func TestALinkReceiptNeedsTheCurrentCredential(t *testing.T) {
	s := linkServer(t)
	r := s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1", cred: 7, receipt: true})
	mustLinkCode(t, r, domain.CodeLinkStale)
	if n := s.count(t, `SELECT COUNT(*) FROM link_fence`); n != 0 {
		t.Fatalf("a stale lookup fenced")
	}
}

// A retention pass that deletes a device's fences raises that device's
// admission generation in the same transaction: an original held on a
// link accepted before the fence is refused once the fence is gone, for a
// member's command and a first join alike, whatever the wall clock reads
// at admission. A new link works, and another device's link is untouched.
func TestAFencedRequestHeldOnAnOldLinkNeverRuns(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbJoin, args: joinChannel("checkout"), op: "jb"}))
	note := linkReq{command: domain.VerbNote, args: map[string]any{"all": true, "body": "held"}, op: "n1", membership: m}
	join := linkReq{key: "k9", command: domain.VerbJoin, args: joinChannel("checkout"), op: "j9"}
	for _, r := range []linkReq{note, join} {
		lookup := r
		lookup.receipt = true
		mustLinkCode(t, s.link(t, lookup), domain.CodeLinkFenced)
	}
	// The daily pass, with the clock moved past the fences' retention,
	// prunes them; the clock then comes back.
	later := time.Now().Add(store.RetentionWindow + 48*time.Hour)
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		_, err := store.PruneRetentionTx(t.Context(), tx, later)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := s.count(t, `SELECT (SELECT COUNT(*) FROM op WHERE scope = 'link') + (SELECT COUNT(*) FROM link_fence)`); n != 0 {
		t.Fatalf("%d fences survived retention", n)
	}
	admission := int64(s.count(t, `SELECT admission_gen FROM device WHERE device_id = 'dev-a'`))
	if admission < 2 {
		t.Fatalf("admission generation %d after pruning the device's fences", admission)
	}
	// dev-b had no fence: its link carries on.
	mustLinkOK(t, s.link(t, linkReq{device: "dev-b", command: domain.VerbWho}))
	for _, r := range []linkReq{note, join} {
		mustLinkCode(t, s.link(t, r), domain.CodeLinkStale)
	}
	if n := s.count(t, `SELECT COUNT(*) FROM message`); n != 0 {
		t.Fatalf("a held note ran")
	}
	if n := s.count(t, `SELECT COUNT(*) FROM device_key WHERE key = 'k9'`); n != 0 {
		t.Fatalf("a held first join enrolled")
	}
	// The device reconnects: a new link carries new requests.
	note.admission, note.op = admission, "n2"
	mustLinkOK(t, s.link(t, note))
}

// After its device enrolled again (a fresh device store, its home restored
// to an older one, #400), the first call of a known key may come without
// a stamp and is admitted against the home's selection, once: for a
// member that moved, and for one that left. A stamp that does not match
// is still refused, and so is a second call without one.
func TestTheFirstCallAfterReenrollmentNeedsNoStamp(t *testing.T) {
	t.Run("moved", func(t *testing.T) {
		s := linkServer(t)
		m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
		moved := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: map[string]any{"channel": "payments", "move": true},
			op: "j2", membership: m})).Membership
		if moved == nil || moved.Generation < 2 {
			t.Fatalf("move selection %+v", moved)
		}
		// Without enrolling again, a stampless call is refused.
		mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, epoch: 2, gen: "g2"}), core.CodeBindingChanged)
		s.fixtureSQL(t, `UPDATE device SET credential_gen = 2 WHERE device_id = 'dev-a'`)
		r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("payments"), op: "j3", cred: 2, gen: "fresh"}))
		if r.Membership == nil || *r.Membership != *moved {
			t.Fatalf("the device did not get the home's selection back: %+v, want %+v", r.Membership, moved)
		}
		mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, cred: 2, gen: "fresh"}), core.CodeBindingChanged)
		mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWho, cred: 2, gen: "fresh", membership: r.Membership}))
	})
	t.Run("a stamp that does not match", func(t *testing.T) {
		s := linkServer(t)
		m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
		s.fixtureSQL(t, `UPDATE device SET credential_gen = 2 WHERE device_id = 'dev-a'`)
		mustLinkCode(t, s.link(t, linkReq{command: domain.VerbWho, cred: 2, gen: "fresh",
			membership: &core.Membership{BindingID: m.BindingID, Generation: m.Generation + 1}}), core.CodeBindingChanged)
	})
	t.Run("left", func(t *testing.T) {
		s := linkServer(t)
		m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
		left := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbLeave, op: "l1", membership: m})).Membership
		if left == nil {
			left = m
		}
		s.fixtureSQL(t, `UPDATE device SET credential_gen = 2 WHERE device_id = 'dev-a'`)
		// A first call that is refused still hands the device the stamp.
		refused := s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("mistyped"), op: "j2", cred: 2, gen: "fresh"})
		if refused.Status == core.ExitOK || refused.Membership == nil || *refused.Membership != *left {
			t.Fatalf("a refused first call: %+v, want the selection %+v", refused, left)
		}
		mustLinkCode(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("payments"), op: "j3", cred: 2, gen: "fresh"}),
			core.CodeBindingChanged)
		r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("payments"), op: "j4", cred: 2, gen: "fresh",
			membership: refused.Membership}))
		if r.Membership == nil || r.Membership.BindingID == left.BindingID {
			t.Fatalf("rejoin selection %+v after %+v", r.Membership, left)
		}
	})
}

// The answer to the first stampless call after re-enrollment is lost: the
// receipt lookup of that call hands over the key's selection, and the next
// call carrying it is admitted.
func TestALostFirstAnswerAfterReenrollmentIsRecoverable(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	left := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbLeave, op: "l1", membership: m})).Membership
	if left == nil {
		left = m
	}
	s.fixtureSQL(t, `UPDATE device SET credential_gen = 2 WHERE device_id = 'dev-a'`)
	first := linkReq{command: domain.VerbJoin, args: joinChannel("mistyped"), op: "j2", cred: 2, gen: "fresh"}
	_ = s.link(t, first) // its answer is lost
	first.receipt = true
	lookup := s.link(t, first)
	if lookup.Membership == nil || *lookup.Membership != *left {
		t.Fatalf("receipt lookup %+v, want the selection %+v", lookup, left)
	}
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("payments"), op: "j3", cred: 2, gen: "fresh",
		membership: lookup.Membership}))
}

// A wait forwarded from a linked device is one read-only snapshot per
// poll, in the link shape: a member's poll answers with its cursor and
// writes nothing, and an unknown field is refused (#400, 12l).
func TestALinkedWaitIsOneSnapshotPerPoll(t *testing.T) {
	s := linkServer(t)
	m := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"})).Membership
	before := s.count(t, `SELECT (SELECT COUNT(*) FROM message) + (SELECT COUNT(*) FROM op)`)
	r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWait, args: map[string]any{"work_only": true}, membership: m}))
	var snap domain.WaitSnapshot
	if err := json.Unmarshal(r.Data, &snap); err != nil || snap.Cursor.BindingID != m.BindingID {
		t.Fatalf("snapshot %s, %v", r.Data, err)
	}
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWait, args: map[string]any{"work_only": true, "cursor": snap.Cursor}, membership: m}))
	if n := s.count(t, `SELECT (SELECT COUNT(*) FROM message) + (SELECT COUNT(*) FROM op)`); n != before {
		t.Fatalf("%d rows written by wait polls", n-before)
	}
	// A wait acknowledges no roster, whatever cursor it carries (Sol #3684).
	mustLinkOK(t, s.link(t, linkReq{key: "other", command: domain.VerbJoin, args: joinChannel("checkout"), op: "j2"}))
	acked := func() int {
		return s.count(t, `SELECT roster_ack_seq FROM binding WHERE id = `+fmt.Sprint(m.BindingID))
	}
	ackBefore := acked()
	mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWait, args: map[string]any{}, membership: m, ack: ^uint64(0)}))
	if got := acked(); got != ackBefore {
		t.Fatalf("a linked wait moved the roster acknowledgement %d -> %d", ackBefore, got)
	}
	if r := s.link(t, linkReq{command: domain.VerbWait, args: map[string]any{"pid": 1}, membership: m}); r.Status == core.ExitOK {
		t.Fatal("a wait with an unknown field was run")
	}
}

// A linked wait counts as running only once a poll carries the cursor
// the home gave it (Sol #3693): if the first answer is lost, an ask stored
// before the retry is not told the wait will wake its recipient, and the
// recipient still finds it in its inbox.
func TestALinkedWaitIsPresentOnlyWithItsCursor(t *testing.T) {
	s := linkServer(t)
	join := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: joinChannel("checkout"), op: "j1"}))
	m := join.Membership
	var joined domain.JoinResult
	if err := json.Unmarshal(join.Data, &joined); err != nil {
		t.Fatal(err)
	}
	asker := mustLinkOK(t, s.link(t, linkReq{key: "other", command: domain.VerbJoin, args: joinChannel("checkout"), op: "j2"})).Membership
	ask := func(op string) domain.Delivery {
		t.Helper()
		r := mustLinkOK(t, s.link(t, linkReq{key: "other", command: domain.VerbAsk, membership: asker, op: op,
			args: map[string]any{"to": []string{joined.Alias}, "body": "work"}}))
		var out domain.AskResult
		if err := json.Unmarshal(r.Data, &out); err != nil || len(out.Recipients) != 1 {
			t.Fatalf("ask %s, %v", r.Data, err)
		}
		return out.Recipients[0]
	}
	wait := func(cursor *domain.WaitCursor) domain.WaitSnapshot {
		t.Helper()
		args := map[string]any{"work_only": true}
		if cursor != nil {
			args["cursor"] = cursor
		}
		var snap domain.WaitSnapshot
		r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWait, args: args, membership: m}))
		if err := json.Unmarshal(r.Data, &snap); err != nil {
			t.Fatal(err)
		}
		return snap
	}
	wait(nil) // its answer is lost on the way back
	if d := ask("a1"); d.Wake.Wake == domain.WakeWait {
		t.Fatalf("an ask was told a wait without a baseline wakes %+v", d)
	}
	retry := wait(nil) // the retry's baseline is past the ask
	inbox := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbInbox, args: map[string]any{}, membership: m}))
	var items domain.InboxResult
	if err := json.Unmarshal(inbox.Data, &items); err != nil || len(items.Unread)+len(items.Owed) == 0 {
		t.Fatalf("the ask is not in the inbox: %s, %v", inbox.Data, err)
	}
	wait(&retry.Cursor) // the client holds its baseline: now the wait runs
	if d := ask("a2"); d.Wake.Wake != domain.WakeWait {
		t.Fatalf("an ask after a cursor poll %+v", d)
	}
	// A new wait starts over: its first poll takes a new baseline, and
	// the earlier wait's presence no longer vouches for it.
	wait(nil)
	if d := ask("a3"); d.Wake.Wake == domain.WakeWait {
		t.Fatalf("a new wait's first poll kept the earlier presence: %+v", d)
	}
}

func TestALinkedWaitRecoversItsBaselineAfterLosingTheFirstAnswer(t *testing.T) {
	s := linkServer(t)
	join := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbJoin, args: map[string]any{
		"channel": "checkout", "alias": "reader",
	}, op: "join-reader"}))
	writer := mustLinkOK(t, s.link(t, linkReq{key: "writer", command: domain.VerbJoin, args: joinChannel("checkout"), op: "join-writer"}))
	ask := func(op string) domain.AskResult {
		t.Helper()
		r := mustLinkOK(t, s.link(t, linkReq{key: "writer", command: domain.VerbAsk, membership: writer.Membership,
			op: op, args: map[string]any{"to": []string{"reader"}, "body": "work"}}))
		var out domain.AskResult
		if err := json.Unmarshal(r.Data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	poll := func(op string, cursor *domain.WaitCursor) domain.WaitSnapshot {
		t.Helper()
		r := mustLinkOK(t, s.link(t, linkReq{command: domain.VerbWait, membership: join.Membership, op: op,
			args: domain.Wait{WorkOnly: true, Cursor: cursor}}))
		var out domain.WaitSnapshot
		if err := json.Unmarshal(r.Data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	old := ask("old")
	attempt := wire.NewID()
	first := poll(wire.WaitAttemptPrefix+attempt, nil)
	if first.Cursor.MessageID != old.ID || first.CursorFingerprint == "" || len(first.Unread) != 0 {
		t.Fatalf("first baseline %+v", first)
	}
	arrived := ask("during-gap")
	if arrived.Recipients[0].Wake.Wake == domain.WakeWait {
		t.Fatal("the wait was advertised before the client got a cursor")
	}
	retry := poll(wire.WaitRetryPrefix+attempt, nil)
	if !retry.InitialCursorRecovered || retry.Cursor != first.Cursor || retry.CursorFingerprint != first.CursorFingerprint {
		t.Fatalf("lost baseline %+v/%q, retry got %+v/%q", first.Cursor, first.CursorFingerprint, retry.Cursor, retry.CursorFingerprint)
	}
	resumed := poll("", &retry.Cursor)
	if len(resumed.Unread) != 1 || resumed.Unread[0].ID != arrived.ID {
		t.Fatalf("resumed wait returned %+v, want only ask %d", resumed.Unread, arrived.ID)
	}
	next := poll(wire.WaitAttemptPrefix+wire.NewID(), nil)
	if next.Cursor.MessageID != arrived.ID || len(next.Unread) != 0 {
		t.Fatalf("new invocation inherited the old baseline: %+v", next)
	}
	missing := s.link(t, linkReq{command: domain.VerbWait, membership: join.Membership,
		op: wire.WaitRetryPrefix + wire.NewID(), args: domain.Wait{WorkOnly: true}})
	mustLinkCode(t, missing, domain.CodeWaitCursorInvalidated)
	if written := s.count(t, `SELECT COUNT(*) FROM op WHERE verb = 'wait'`); written != 0 {
		t.Fatalf("wait recorded %d operations", written)
	}
}
